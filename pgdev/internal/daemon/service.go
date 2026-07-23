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
	"sync"

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

// Service implements agentapi.Service. Two-machine model (spec 0002): one
// Service = one machine = one backend (its Slot). Active/staging roles and
// promote live on the host; this daemon never reasons about a sibling.
type Service struct {
	Cfg     config.Config
	Store   *store.Store
	Incus   *backend.Incus
	Ops     *ops.Ops
	Journal task.Journal
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
		Ver:     version,
		Log:     logx.Or(log),
	}, nil
}

// slot is this daemon's backend slot (a|b), from PG_SLOT. Defaults to "a" for a
// legacy/unset environment so a single-machine deploy still resolves a backend.
func (s *Service) slot() string {
	if s.Cfg.Slot == "b" {
		return "b"
	}
	return "a"
}

// container is this machine's one backend container name.
func (s *Service) container() string { return s.Cfg.Container(s.slot()) }

// RecoverPending replays any interrupted mutation. Called once on daemon start.
func (s *Service) RecoverPending(ctx context.Context) error {
	return task.Recover(ctx, s.Journal, s.Ops.Registry())
}

func (s *Service) Version() agentapi.VersionResponse {
	return agentapi.VersionResponse{Version: s.Ver, APIVersion: agentapi.APIVersion}
}

// ----- status --------------------------------------------------------------

func (s *Service) Status(ctx context.Context) (agentapi.StatusResponse, error) {
	slot := s.slot()
	c := s.container()
	state, ips, err := s.Incus.Info(ctx, c)
	if err != nil {
		return agentapi.StatusResponse{}, err
	}
	snaps, err := s.snapshotInfos(slot)
	if err != nil {
		return agentapi.StatusResponse{}, err
	}
	fwd := false
	if state == "RUNNING" {
		fwd = s.Incus.HasProxyDevice(ctx, c, blueprint.ForwardDevice)
	}
	return agentapi.StatusResponse{
		Slot:             slot,
		Container:        c,
		State:            state,
		IPs:              ips,
		BackendPort:      s.Cfg.BackendPort,
		ProxyDevice:      fwd,
		DataStoreMounted: s.Store.RequireMounted() == nil,
		IncusVersion:     s.Incus.Version(ctx),
		Snapshots:        snaps,
	}, nil
}

func (s *Service) Snapshots(ctx context.Context) ([]agentapi.SnapshotInfo, error) {
	return s.snapshotInfos(s.slot())
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

// ----- reconcile -----------------------------------------------------------

// Reconcile re-asserts this backend's eth0 forward device from current state.
// It takes the single-mutation lock so it can't race a start/stop.
func (s *Service) Reconcile(ctx context.Context) (agentapi.ReconcileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.reconcile(ctx)
	return agentapi.ReconcileResponse{BackendRunning: res.BackendRunning, Actions: res.Actions}, err
}

// reconcile builds this machine's blueprint and applies it.
func (s *Service) reconcile(ctx context.Context) (reconcile.Result, error) {
	bp := blueprint.Compute(s.Cfg, s.slot())
	return reconcile.Reconcile(ctx, s.Incus, bp, s.Log)
}

// ----- backend lifecycle ---------------------------------------------------

// Start brings this backend fully up — container running AND PostgreSQL ready —
// then re-asserts its eth0 forward so the host can reach it. Role-agnostic: the
// host decides whether this machine is active or staging.
func (s *Service) Start(ctx context.Context) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.container()
	if err := s.Incus.StartContainerAndWait(ctx, c); err != nil {
		return agentapi.OpResponse{}, err
	}
	if _, err := s.reconcile(ctx); err != nil {
		return agentapi.OpResponse{}, err
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("started %s (PostgreSQL ready)", c)}, nil
}

// Stop stops this backend. StopContainer refuses if it won't reach STOPPED.
func (s *Service) Stop(ctx context.Context) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.container()
	if err := s.Incus.StopContainer(ctx, c); err != nil {
		return agentapi.OpResponse{}, err
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("stopped %s", c)}, nil
}

// ----- snapshot / restore --------------------------------------------------

func (s *Service) Snapshot(ctx context.Context, req agentapi.SnapshotRequest) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPending(ctx); err != nil {
		return agentapi.OpResponse{}, err
	}
	slot := s.slot()
	t, err := s.Ops.Snapshot(ctx, slot, req.Name, req.Force)
	if err != nil {
		return agentapi.OpResponse{}, err
	}
	if err := task.Run(ctx, s.Journal, t); err != nil {
		return agentapi.OpResponse{}, err
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("snapshot %q created on %s", req.Name, s.container())}, nil
}

func (s *Service) Restore(ctx context.Context, req agentapi.RestoreRequest) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.RecoverPending(ctx); err != nil {
		return agentapi.OpResponse{}, err
	}
	slot := s.slot()

	var (
		t    task.Task
		err  error
		name = req.Name
	)
	if req.Last {
		t, err = s.Ops.RestoreLast(ctx, slot)
		if name == "" {
			name, _ = s.Store.Last(slot)
		}
	} else {
		if req.Name == "" {
			return agentapi.OpResponse{}, fmt.Errorf("name is required")
		}
		t, err = s.Ops.Restore(ctx, slot, req.Name)
	}
	if err != nil {
		return agentapi.OpResponse{}, err
	}

	// The newer-timeline confirmation is the host's job (on its TTY). Refuse a
	// destructive restore only when the host did not explicitly resolve it.
	if !req.Force {
		after, aerr := s.Store.After(slot, name)
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
	return agentapi.OpResponse{Message: fmt.Sprintf("restored %s to snapshot %q", s.container(), name)}, nil
}
