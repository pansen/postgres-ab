// Package daemon is the machine-side orchestration behind the HTTP API. It owns
// the single-mutation mutex and boot-time recovery centrally (§5.8 of
// issues/0001): every mutation acquires the lock and first replays any journal
// record a killed predecessor left behind, so "crash mid-restore" auto-heals.
//
// It composes the Slice 1 pieces (store, backend, ops, task) with the Slice 2
// blueprint + reconcile, and exposes them as the agentapi.Service the HTTP layer
// serves. Status returns raw facts only; the host renders credentials/endpoints.
package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"pansen.me/pgdev/internal/activeslot"
	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/backend"
	"pansen.me/pgdev/internal/blueprint"
	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/logx"
	"pansen.me/pgdev/internal/ops"
	"pansen.me/pgdev/internal/reconcile"
	"pansen.me/pgdev/internal/store"
	"pansen.me/pgdev/internal/task"
)

// Service implements agentapi.Service.
type Service struct {
	Cfg     config.Config
	Store   *store.Store
	Incus   *backend.Incus
	Ops     *ops.Ops
	Journal task.Journal
	Active  activeslot.Pointer
	Ver     string
	Log     logx.Func

	mu sync.Mutex // single mutation at a time (today's flock)
}

// New wires a Service from a resolved config. version is the build stamp the
// deploy handshake checks.
func New(cfg config.Config, version string, log logx.Func) (*Service, error) {
	st := store.NewOSStore(cfg.DataRoot)
	j, err := task.NewFileJournal(st.JournalDir())
	if err != nil {
		return nil, err
	}
	be := backend.NewIncus(cfg)
	be.Log = log
	o := ops.New(st, be, cfg)
	o.Log = log
	return &Service{
		Cfg:     cfg,
		Store:   st,
		Incus:   be,
		Ops:     o,
		Journal: j,
		Active:  activeslot.Pointer{Path: cfg.ActiveSlotPath(), UID: cfg.HostUID, GID: cfg.HostGID},
		Ver:     version,
		Log:     logx.Or(log),
	}, nil
}

// RecoverPending replays any interrupted mutation. Called once on daemon start.
func (s *Service) RecoverPending(ctx context.Context) error {
	return task.Recover(ctx, s.Journal, s.Ops.Registry())
}

func (s *Service) Version() agentapi.VersionResponse {
	return agentapi.VersionResponse{Version: s.Ver, APIVersion: agentapi.APIVersion}
}

// ----- status --------------------------------------------------------------

func (s *Service) Status(ctx context.Context) (agentapi.StatusResponse, error) {
	active := s.Active.Get()
	staging := other(active)
	proxyState, _ := s.Incus.State(ctx, s.Cfg.ProxyName)

	out := agentapi.StatusResponse{
		Active:           active,
		ProxyName:        s.Cfg.ProxyName,
		ProxyState:       proxyState,
		DataStoreMounted: s.Store.RequireMounted() == nil,
		IncusVersion:     s.Incus.Version(ctx),
	}
	// Active first, then staging — matches the shell's two-row table.
	for _, sr := range []struct{ slot, role string }{{active, "active"}, {staging, "staging"}} {
		c := s.Cfg.Container(sr.slot)
		state, ips, err := s.Incus.Info(ctx, c)
		if err != nil {
			return out, err
		}
		snaps, err := s.snapshotInfos(sr.slot)
		if err != nil {
			return out, err
		}
		out.Backends = append(out.Backends, agentapi.BackendStatus{
			Slot: sr.slot, Container: c, Role: sr.role, State: state, IPs: ips, Snapshots: snaps,
		})
	}
	return out, nil
}

func (s *Service) Snapshots(ctx context.Context, slot string) ([]agentapi.SnapshotInfo, error) {
	if slot != "a" && slot != "b" {
		return nil, fmt.Errorf("slot must be a or b (got %q)", slot)
	}
	return s.snapshotInfos(slot)
}

func (s *Service) snapshotInfos(slot string) ([]agentapi.SnapshotInfo, error) {
	snaps, err := s.Store.List(slot)
	if err != nil {
		return nil, err
	}
	out := make([]agentapi.SnapshotInfo, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, agentapi.SnapshotInfo{Name: sn.Name, CreatedUnix: sn.ModUnix})
	}
	return out, nil
}

// ----- reconcile & promote -------------------------------------------------

// Reconcile re-asserts backend IP pins and proxy forwards from current state.
// It takes the single-mutation lock (the shell ran `refresh` under the same
// flock) so it can't race a concurrent promote's own reconcile.
func (s *Service) Reconcile(ctx context.Context) (agentapi.ReconcileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.reconcile(ctx, s.Active.Get())
	return agentapi.ReconcileResponse{ProxyRunning: res.ProxyRunning, Actions: res.Actions}, err
}

// reconcile builds the blueprint for `active` and applies it.
func (s *Service) reconcile(ctx context.Context, active string) (reconcile.Result, error) {
	aIP, bIP, err := s.resolveBackendIPs(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	bp := blueprint.Compute(s.Cfg, active, aIP, bIP)
	return reconcile.Reconcile(ctx, s.Incus, bp, s.Log)
}

// resolveBackendIPs turns configured overrides (or the incusbr0-derived
// defaults) into concrete pinned addresses, mirroring _pick_backend_{a,b}_ip.
func (s *Service) resolveBackendIPs(ctx context.Context) (aIP, bIP string, err error) {
	aIP, bIP = s.Cfg.BackendAIP, s.Cfg.BackendBIP
	if aIP != "" && bIP != "" {
		return aIP, bIP, nil
	}
	prefix, err := s.Incus.NetworkIPv4Prefix(ctx)
	if err != nil {
		return "", "", err
	}
	if aIP == "" {
		aIP = prefix + ".11"
	}
	if bIP == "" {
		bIP = prefix + ".12"
	}
	return aIP, bIP, nil
}

// Promote flips active↔staging and re-points both forwards. With the reconcile
// collapse this is "flip the pointer, then Reconcile()"; on a reconcile failure
// it flips back and reconciles again — the whole hand-rolled rollback from
// cmd_promote reduces to this.
func (s *Service) Promote(ctx context.Context) (agentapi.PromoteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPending(ctx); err != nil {
		return agentapi.PromoteResponse{}, err
	}

	from := s.Active.Get()
	to := other(from)
	if err := s.promotable(ctx, from, to); err != nil {
		return agentapi.PromoteResponse{}, err
	}

	s.Log("promoting: active %s → %s", from, to)
	if err := s.Active.Set(to); err != nil {
		return agentapi.PromoteResponse{}, err
	}
	if _, err := s.reconcile(ctx, to); err != nil {
		s.Log("proxy update failed; restoring active slot %s", from)
		_ = s.Active.Set(from)
		if _, rerr := s.reconcile(ctx, from); rerr != nil {
			return agentapi.PromoteResponse{}, fmt.Errorf("promote failed and rollback also failed: %w (run 'pgdev refresh'); original: %v", rerr, err)
		}
		return agentapi.PromoteResponse{}, fmt.Errorf("promote failed, rolled back to %s: %w", from, err)
	}

	st, err := s.Status(ctx)
	return agentapi.PromoteResponse{From: from, To: to, Status: st}, err
}

// promotable enforces cmd_promote's precondition: proxy and both backends RUNNING.
func (s *Service) promotable(ctx context.Context, from, to string) error {
	var problems []string
	if st, _ := s.Incus.State(ctx, s.Cfg.ProxyName); st != "RUNNING" {
		problems = append(problems, fmt.Sprintf("%s is %s — run 'make start'", s.Cfg.ProxyName, orAbsent(st)))
	}
	if st, _ := s.Incus.State(ctx, s.Cfg.Container(from)); st != "RUNNING" {
		problems = append(problems, fmt.Sprintf("%s (active) is %s — run 'make start'", s.Cfg.Container(from), orAbsent(st)))
	}
	if st, _ := s.Incus.State(ctx, s.Cfg.Container(to)); st != "RUNNING" {
		problems = append(problems, fmt.Sprintf("%s (staging) is %s — run 'make pg.staging.start'", s.Cfg.Container(to), orAbsent(st)))
	}
	if len(problems) > 0 {
		return fmt.Errorf("promotion requires proxy and both backends RUNNING:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ----- snapshot / restore --------------------------------------------------

func (s *Service) Snapshot(ctx context.Context, req agentapi.SnapshotRequest) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPending(ctx); err != nil {
		return agentapi.OpResponse{}, err
	}
	if req.Slot != "a" && req.Slot != "b" {
		return agentapi.OpResponse{}, fmt.Errorf("slot must be a or b (got %q)", req.Slot)
	}
	t, err := s.Ops.Snapshot(ctx, req.Slot, req.Name, req.Force)
	if err != nil {
		return agentapi.OpResponse{}, err
	}
	if err := task.Run(ctx, s.Journal, t); err != nil {
		return agentapi.OpResponse{}, err
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("snapshot %q created on %s", req.Name, s.Cfg.Container(req.Slot))}, nil
}

func (s *Service) Restore(ctx context.Context, req agentapi.RestoreRequest) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPending(ctx); err != nil {
		return agentapi.OpResponse{}, err
	}
	if req.Slot != "a" && req.Slot != "b" {
		return agentapi.OpResponse{}, fmt.Errorf("slot must be a or b (got %q)", req.Slot)
	}

	var (
		t    task.Task
		err  error
		name = req.Name
	)
	if req.Last {
		t, err = s.Ops.RestoreLast(ctx, req.Slot)
		if name == "" {
			name, _ = s.Store.Last(req.Slot)
		}
	} else {
		if req.Name == "" {
			return agentapi.OpResponse{}, fmt.Errorf("name is required")
		}
		t, err = s.Ops.Restore(ctx, req.Slot, req.Name)
	}
	if err != nil {
		return agentapi.OpResponse{}, err
	}

	// The newer-timeline confirmation is the host's job (on its TTY). Refuse a
	// destructive restore only when the host did not explicitly resolve it.
	if !req.Force {
		after, aerr := s.Store.After(req.Slot, name)
		if aerr != nil {
			return agentapi.OpResponse{}, aerr
		}
		if len(after) > 0 {
			return agentapi.OpResponse{}, fmt.Errorf("restoring %q would delete %d newer snapshot(s); re-run with --force", name, len(after))
		}
	}

	if err := task.Run(ctx, s.Journal, t); err != nil {
		return agentapi.OpResponse{}, err
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("restored %s to snapshot %q", s.Cfg.Container(req.Slot), name)}, nil
}

func other(slot string) string {
	if slot == "a" {
		return "b"
	}
	return "a"
}

func orAbsent(state string) string {
	if state == "" {
		return "ABSENT"
	}
	return state
}
