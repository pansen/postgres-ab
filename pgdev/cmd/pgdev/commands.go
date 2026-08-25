package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/applecli"
)

// ----- lifecycle (up / down) -----------------------------------------------

// upCmd provisions the backends that are MISSING, per machine — it is
// idempotent, not all-or-nothing. `make start` runs it after `deploy` so the
// state left by `pg.staging.purge` (machine recreated by `make machine`,
// pgdevd deployed, but no Incus backend on it) heals itself: without this,
// start left the slot at ABSENT and every staging target failed with Incus's
// raw `Instance not found`. A slot that already has a container is left
// strictly alone — the daemon's Up still refuses to touch one (`run down
// first`), so no existing data can be provisioned over.
func (a *app) upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Provision any missing machine backend (vpg-a, vpg-b); existing ones are left alone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.cfg.RequireCreds(); err != nil {
				return err
			}
			provisioned := 0
			for _, slot := range slotsAB {
				machine := a.cfg.MachineNameForSlot(slot)
				// Probe with the short-timeout client: an unreachable machine
				// must fail fast here rather than hang on the 30-minute Up.
				probe, err := a.clientFor(ctx, slot)
				if err != nil {
					return err
				}
				st, err := probe.Status(ctx)
				if err != nil {
					return fmt.Errorf("%s: %w", machine, err)
				}
				if st.State != "" {
					a.log.Info("backend already provisioned — skipping",
						"machine", machine, "container", st.Container, "state", st.State)
					continue
				}
				cl, err := a.longClientFor(ctx, slot)
				if err != nil {
					return err
				}
				a.log.Info("provisioning backend (the first run builds the golden PostgreSQL image; this can take a few minutes)",
					"machine", machine)
				if _, err := cl.Up(ctx); err != nil {
					return fmt.Errorf("%s: %w", machine, err)
				}
				provisioned++
			}
			// Default to "a" active the first time the pointer file has never
			// been written, so a fresh `pgdev up` has a well-defined role split.
			if _, err := os.Stat(a.cfg.ActiveMachinePath()); os.IsNotExist(err) {
				if err := a.setActive(ctx, "a"); err != nil {
					return err
				}
			}
			if provisioned == 0 {
				a.log.Info("both backends already provisioned — nothing to do")
				return nil
			}
			a.reconcileProxyIfInstalled(ctx, "up")
			a.log.Info("pg-dev ready")
			fmt.Println()
			a.renderStatus(ctx)
			return nil
		},
	}
}

func (a *app) downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Delete both machines' backend containers and XFS data trees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			for _, slot := range slotsAB {
				machine := a.cfg.MachineNameForSlot(slot)
				cl, err := a.longClientFor(ctx, slot)
				if err != nil {
					a.log.Warn("skipping machine", "machine", machine, "err", err)
					continue
				}
				res, err := cl.Down(ctx)
				if err != nil {
					a.log.Warn("down failed", "machine", machine, "err", err)
					continue
				}
				a.log.Info(res.Message, "machine", machine)
			}
			return nil
		},
	}
}

// ----- status ----------------------------------------------------------------

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Active/staging machine roles, per-machine state/endpoints, and snapshot timelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.renderStatus(cmd.Context())
			return nil
		},
	}
}

// slotStatus pairs one machine's /v1/status result with any error reaching it,
// so status/snapshots rendering can show an unreachable machine instead of
// failing the whole command.
type slotStatus struct {
	st  agentapi.StatusResponse
	err error
	// absent is set when the machine itself is gone (never created, or deleted
	// by `pg.staging.purge`). That is an expected steady state, not a fault, so
	// status reports it as ABSENT rather than UNREACHABLE.
	absent bool
}

// fetchStatuses queries both machines' status, tolerating per-machine errors.
func (a *app) fetchStatuses(ctx context.Context) map[string]slotStatus {
	out := make(map[string]slotStatus, len(slotsAB))
	for _, slot := range slotsAB {
		out[slot] = a.fetchStatus(ctx, slot)
	}
	return out
}

// fetchStatus queries one machine's status, turning any failure into a
// slotStatus rather than an error, so a caller can report the fault in place.
func (a *app) fetchStatus(ctx context.Context, slot string) slotStatus {
	cl, err := a.clientFor(ctx, slot)
	if err == nil {
		var st agentapi.StatusResponse
		if st, err = cl.Status(ctx); err == nil {
			return slotStatus{st: st}
		}
	}
	// Only ask the (slow) Apple CLI whether the machine exists once something
	// already went wrong — the happy path stays exec-free.
	return slotStatus{err: err, absent: !a.apple(slot).Exists(ctx)}
}

func (a *app) renderStatus(ctx context.Context) {
	statuses := a.fetchStatuses(ctx)
	active := a.active.Get()
	staging := a.active.Staging()

	for _, slot := range slotsAB {
		if v := statuses[slot].st.IncusVersion; v != "" {
			fmt.Println(v)
			fmt.Println()
			break
		}
	}

	fmt.Printf("active machine: %s   (active=%s, staging=%s)\n",
		a.cfg.MachineNameForSlot(active), a.cfg.MachineNameForSlot(active), a.cfg.MachineNameForSlot(staging))
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tMACHINE\tCONTAINER\tSTATE\tCLIENT-ENDPOINT\tSNAPSHOTS\tIPs")
	for _, role := range []string{"active", "staging"} {
		slot := a.roleSlot(role)
		ms := statuses[slot]
		machine := a.cfg.MachineNameForSlot(slot)
		endpoint := fmt.Sprintf("%s:%d", a.cfg.ProxyHostname, a.cfg.ClientPort(role))
		if ms.err != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", role, machine, "-", statusState(ms), endpoint, "-", "-")
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			role, machine, orDash(ms.st.Container), statusState(ms), endpoint, len(ms.st.Snapshots), orDash(strings.Join(ms.st.IPs, ",")))
	}
	tw.Flush()
	fmt.Println()
	// A machine whose bootstrap failed answers every request normally but cannot
	// provision or start a backend — say so here rather than letting it surface
	// later as an unrelated-looking Incus error.
	for _, slot := range slotsAB {
		if e := statuses[slot].st.BootstrapError; e != "" {
			a.log.Warn("machine bootstrap failed — provisioning will not work until this is fixed",
				"machine", a.cfg.MachineNameForSlot(slot), "err", e)
		}
	}
	a.renderProxy(ctx)
	fmt.Println()
	a.renderSnapshots(statuses, true)
}

// ----- snapshots -------------------------------------------------------------

func (a *app) snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots",
		Short: "Snapshot timelines for active and staging",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.renderSnapshots(a.fetchStatuses(cmd.Context()), false)
			return nil
		},
	}
}

func (a *app) renderSnapshots(statuses map[string]slotStatus, withPsql bool) {
	for _, role := range []string{"active", "staging"} {
		slot := a.roleSlot(role)
		ms := statuses[slot]
		machine := a.cfg.MachineNameForSlot(slot)
		fmt.Printf("─── %-7s (%s) ───\n", role, machine)
		if ms.absent {
			fmt.Printf("(machine %s does not exist — run '%s' to bring it back)\n\n", machine, rebuildHint(role))
			continue
		}
		if ms.err != nil {
			fmt.Printf("(unreachable: %v)\n\n", ms.err)
			continue
		}
		if withPsql {
			fmt.Printf("$ %s\n\n", a.psqlCmd(a.cfg.ClientPort(role)))
		}
		fmt.Printf("%s\n\n", dbSizeLine(a.cfg.PGDB, ms.st.DBSize))
		if t := snapshotTable(ms.st.Snapshots); t != "" {
			fmt.Print(t)
		} else {
			fmt.Println("(no snapshots)")
		}
		fmt.Println()
	}
}

// snapshotTable renders the snapshot timeline as a column-aligned block (empty
// when there are none). It returns the text rather than printing it so callers
// that need it inside an indented prompt can shift the whole block without
// disturbing tabwriter's alignment.
func snapshotTable(snaps []agentapi.SnapshotInfo) string {
	if len(snaps) == 0 {
		return ""
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCREATED_AT")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\n", s.Name, time.Unix(s.CreatedUnix, 0).Format("2006-01-02 15:04:05 -0700"))
	}
	tw.Flush()
	return b.String()
}

// ----- endpoint & ip ---------------------------------------------------------

func (a *app) endpointCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "endpoint",
		Short: "Print client endpoints, .pgpass lines, and psql commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.cfg.RequireCreds(); err != nil {
				return err
			}
			h := a.cfg.ProxyHostname
			fmt.Printf("active   host=%s port=%d dbname=%s   (current data, machine %s)\n",
				h, a.cfg.ClientActivePort, a.cfg.PGDB, a.cfg.MachineNameForSlot(a.active.Get()))
			fmt.Printf("staging  host=%s port=%d dbname=%s   (import target, machine %s)\n\n",
				h, a.cfg.ClientStagingPort, a.cfg.PGDB, a.cfg.MachineNameForSlot(a.active.Staging()))
			fmt.Println(".pgpass lines:")
			fmt.Printf("%s:%d:*:%s:%s\n", h, a.cfg.ClientActivePort, a.cfg.PGUser, a.cfg.PGPassword)
			fmt.Printf("%s:%d:*:%s:%s\n\n", h, a.cfg.ClientStagingPort, a.cfg.PGUser, a.cfg.PGPassword)
			fmt.Println("psql commands:")
			fmt.Printf("  active:  %s\n", a.psqlCmd(a.cfg.ClientActivePort))
			fmt.Printf("  staging: %s\n", a.psqlCmd(a.cfg.ClientStagingPort))
			fmt.Printf("\nnote: these ports are served by the socat client proxy ('make proxy.install'); it\n")
			fmt.Printf("      relays to each machine's IP, which may drift on reboot ('pgdev refresh' re-points it).\n")
			return nil
		},
	}
}

func (a *app) ipCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ip",
		Short: "Both machines' IPs and client endpoints",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ROLE\tMACHINE\tIP\tCLIENT-ENDPOINT")
			for _, role := range []string{"active", "staging"} {
				slot := a.roleSlot(role)
				ip := a.machineIP(ctx, slot)
				a.writeMachineIPFile(ctx, slot, ip)
				endpoint := fmt.Sprintf("%s:%d", a.cfg.ProxyHostname, a.cfg.ClientPort(role))
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", role, a.cfg.MachineNameForSlot(slot), orQ(ip), endpoint)
			}
			tw.Flush()
			return nil
		},
	}
}

// ----- promote & refresh -----------------------------------------------------

func (a *app) promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote",
		Short: "Flip active↔staging (re-point the client proxy, no data moves)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			from := a.active.Get()
			to := a.active.Staging()

			// Both backends must be RUNNING before flipping: promoting onto a
			// machine whose backend isn't up would hand clients a dead :5442.
			for _, slot := range slotsAB {
				machine := a.cfg.MachineNameForSlot(slot)
				cl, err := a.clientFor(ctx, slot)
				if err != nil {
					return fmt.Errorf("promote: %w — run 'make start'", err)
				}
				st, err := cl.Status(ctx)
				if err != nil {
					return fmt.Errorf("promote: %s: %w — run 'make start'", machine, err)
				}
				if st.State != "RUNNING" {
					fix := "run 'make start'"
					switch {
					case st.State == "": // never provisioned
						fix = "run 'make pg.up'"
					case slot == to: // staging backend merely stopped
						fix = "run 'make pg.staging.start'"
					}
					return fmt.Errorf("promote: %s backend is %s, not RUNNING — %s", machine, orAbsent(st.State), fix)
				}
			}

			// Promote is a pointer write plus a proxy reconcile: no data moves and
			// no daemon call.
			if err := a.setActive(ctx, to); err != nil {
				return err
			}
			// socat serves the client ports (5442/5443) and cannot re-point itself,
			// so re-point it synchronously (DB-driven, flock-guarded, verified) if
			// installed. Sessions on the demoted machine are dropped; clients
			// reconnect onto the new active.
			a.reconcileProxyIfInstalled(ctx, "promote")

			a.log.Info("promoted",
				"active", a.cfg.MachineNameForSlot(to), "staging", a.cfg.MachineNameForSlot(from))
			fmt.Println()
			a.renderStatus(ctx)
			return nil
		},
	}
}

func (a *app) refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-discover both machine IPs and re-point the client proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			for _, slot := range slotsAB {
				machine := a.cfg.MachineNameForSlot(slot)
				ip := a.machineIP(ctx, slot)
				a.writeMachineIPFile(ctx, slot, ip)
				if ip == "" {
					a.log.Warn("no IP (machine down?) — skipping reconcile", "machine", machine)
					continue
				}
				cl, err := a.clientFor(ctx, slot)
				if err != nil {
					a.log.Warn("machine unreachable", "machine", machine, "err", err)
					continue
				}
				res, err := cl.Reconcile(ctx)
				if err != nil {
					a.log.Error("backend reconcile failed", "machine", machine, "err", err)
					continue
				}
				a.log.Info("backend reconciled", "machine", machine, "running", res.BackendRunning)
				for _, act := range res.Actions {
					a.log.Info("backend action", "machine", machine, "action", act)
				}
			}
			// Re-point the socat proxy at the freshly-discovered IPs (only if it's
			// installed; socat can't re-point itself).
			a.reconcileProxyIfInstalled(ctx, "refresh")
			a.log.Info("endpoints re-pointed",
				"active", fmt.Sprintf("%s:%d → %s", a.cfg.ProxyHostname, a.cfg.ClientActivePort, a.cfg.MachineNameForSlot(a.active.Get())),
				"staging", fmt.Sprintf("%s:%d → %s", a.cfg.ProxyHostname, a.cfg.ClientStagingPort, a.cfg.MachineNameForSlot(a.active.Staging())))
			return nil
		},
	}
}

// ----- snapshot / restore (active + staging) --------------------------------

func (a *app) snapshotCmd(role string) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "snapshot <name>",
		Short: "Create an XFS reflink snapshot on the " + role + " backend",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Long-timeout client: a snapshot stops PostgreSQL (a checkpoint of a
			// freshly-imported dump alone can outlast 30s) and reflink-clones the
			// whole data dir before answering. The default deadline aborted the
			// request while the daemon went on to complete it — the caller saw a
			// failure for a snapshot that exists.
			cl, err := a.longClientForRole(ctx, role)
			if err != nil {
				return err
			}
			res, err := cl.Snapshot(ctx, agentapi.SnapshotRequest{Name: args[0], Force: force})
			if err != nil {
				return err
			}
			a.log.Info(res.Message, "machine", a.cfg.MachineNameForSlot(a.roleSlot(role)), "snapshot", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "replace an existing same-named snapshot")
	return c
}

func (a *app) restoreCmd(role string) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "restore <name>",
		Short: "Restore the " + role + " backend to a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRestore(cmd.Context(), role, args[0], false, force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "delete newer snapshots without confirmation")
	return c
}

func (a *app) restoreLastCmd(role string) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "restore-last",
		Short: "Restore the " + role + " backend to its most recent snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runRestore(cmd.Context(), role, "", true, force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "delete newer snapshots without confirmation")
	return c
}

// runRestore resolves the target, confirms any newer-timeline loss on the host's
// TTY (the daemon never prompts), then issues the restore with an explicit force.
func (a *app) runRestore(ctx context.Context, role, name string, last, force bool) error {
	slot := a.roleSlot(role)
	machine := a.cfg.MachineNameForSlot(slot)
	cl, err := a.clientForRole(ctx, role)
	if err != nil {
		return err
	}
	snaps, err := cl.Snapshots(ctx)
	if err != nil {
		return err
	}
	target := name
	if last {
		if len(snaps.Snapshots) == 0 {
			return fmt.Errorf("no snapshots on %s", machine)
		}
		target = snaps.Snapshots[len(snaps.Snapshots)-1].Name
		a.log.Info("restoring to most recent snapshot", "machine", machine, "snapshot", target)
	}

	after := snapshotsAfter(snaps.Snapshots, target)
	effForce := force
	if len(after) > 0 {
		fmt.Printf("==> Snapshots on %s that will be deleted:\n     %s\n",
			machine, strings.Join(after, "\n     "))
		if !force {
			ok, err := confirm()
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("aborted")
			}
			effForce = true
		}
	}

	// The listing above already proved the machine answers, so the restore itself
	// runs on the long-timeout client: it stops PostgreSQL, swaps the data dir and
	// waits for the cluster to come back, which outlasts the default deadline.
	mut, err := a.longClientForRole(ctx, role)
	if err != nil {
		return err
	}
	res, err := mut.Restore(ctx, agentapi.RestoreRequest{Name: name, Last: last, Force: effForce})
	if err != nil {
		return err
	}
	a.log.Info(res.Message, "machine", machine)
	return nil
}

// ----- staging group ---------------------------------------------------------

func (a *app) stagingCmd() *cobra.Command {
	c := &cobra.Command{Use: "staging", Short: "Operate on the staging (non-active) machine"}
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Restore staging to its 'initial' snapshot (soft reset — in-machine reflink)",
		Args:  cobra.NoArgs,
	}
	var resetForce bool
	reset.Flags().BoolVar(&resetForce, "force", false, "delete newer snapshots without confirmation")
	reset.RunE = func(cmd *cobra.Command, _ []string) error {
		return a.runRestore(cmd.Context(), "staging", "initial", false, resetForce)
	}
	c.AddCommand(
		a.snapshotCmd("staging"),
		a.restoreCmd("staging"),
		a.restoreLastCmd("staging"),
		reset,
		a.stagingStartCmd(),
		a.stagingStopCmd(),
		a.stagingPurgeCmd(),
		a.stagingRebuildCmd(),
	)
	return c
}

// stagingStartCmd brings the staging backend fully up (container + PostgreSQL,
// waiting for readiness). Uses the long-timeout client — bring-up waits on
// systemd and PostgreSQL, which can outlast the default HTTP deadline.
func (a *app) stagingStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the staging backend (container + PostgreSQL, waits for readiness)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			machine := a.cfg.MachineNameForSlot(a.roleSlot("staging"))
			// A machine can exist with no backend on it (e.g. after
			// `pg.staging.purge` when `make start` was interrupted before
			// provisioning). Say so, instead of letting Incus answer with a
			// bare `Failed to fetch instance … Instance not found`.
			probe, err := a.clientForRole(ctx, "staging")
			if err != nil {
				return err
			}
			if st, err := probe.Status(ctx); err == nil && st.State == "" {
				return fmt.Errorf("staging backend %s on %s is not provisioned — run 'make start' (or 'make pg.staging.rebuild' for a fresh machine)",
					st.Container, machine)
			}
			cl, err := a.longClientForRole(ctx, "staging")
			if err != nil {
				return err
			}
			res, err := cl.Start(ctx)
			if err != nil {
				return err
			}
			a.log.Info(res.Message, "machine", machine)
			return nil
		},
	}
}

func (a *app) stagingStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the staging backend",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.clientForRole(ctx, "staging")
			if err != nil {
				return err
			}
			res, err := cl.Stop(ctx)
			if err != nil {
				return err
			}
			a.log.Info(res.Message, "machine", a.cfg.MachineNameForSlot(a.roleSlot("staging")))
			return nil
		},
	}
}

// stagingForDestruction resolves the staging slot and machine names and asserts
// staging is NOT the active machine — the load-bearing safety property shared by
// both destructive staging tiers (rebuild, purge). Structurally Staging() is the
// pointer's complement so they always differ, but this guards the one invariant
// that, if ever violated, would nuke live data — so it is asserted explicitly.
func (a *app) stagingForDestruction() (slot, machine, activeMachine string, err error) {
	slot = a.active.Staging()
	machine = a.cfg.MachineNameForSlot(slot)
	activeMachine = a.cfg.MachineNameForSlot(a.active.Get())
	if machine == activeMachine {
		return "", "", "", fmt.Errorf("refusing to operate on %s — it is the active machine", machine)
	}
	return slot, machine, activeMachine, nil
}

// stagingPurgeCmd is the reclaim-WITHOUT-rebuild tier: delete ONLY the staging
// machine — reclaiming its grown sparse macOS disk — and LEAVE IT DOWN. Unlike
// rebuild it does not recreate/deploy/provision; staging stays gone until you
// `rebuild` (or `make start`) it back. The active machine is never touched. It
// also forgets staging's now-dead IP and drops its socat listener, so the
// endpoint honestly reads "down" instead of dialing a corpse.
func (a *app) stagingPurgeCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "purge",
		Short: "Delete the staging machine to reclaim its macOS disk and leave it DOWN (no rebuild)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			slot, machine, activeMachine, err := a.stagingForDestruction()
			if err != nil {
				return err
			}

			fmt.Printf("==> This DELETES %s (staging), discarding its data and snapshots, and reclaims its macOS disk.\n\n", machine)
			a.printPurgeInventory(ctx, slot, machine)
			fmt.Printf("    It will NOT be recreated — run 'make pg.staging.rebuild' or 'make start' to bring it back.\n\n")
			if !force {
				ok, err := confirm()
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("aborted")
				}
			}

			cli := a.apple(slot)
			a.log.Info("stopping and deleting the machine (this reclaims its macOS disk)", "machine", machine)
			if err := cli.Delete(ctx); err != nil {
				return err
			}

			// Forget staging's now-invalid IP and drop its socat listener, so
			// :5443 reads "down" rather than dialing the deleted machine.
			if db, err := a.track(ctx); err == nil {
				if err := db.ForgetMachine(ctx, slot); err != nil {
					a.log.Warn("forgetting staging machine failed", "slot", slot, "err", err)
				}
			}
			a.reconcileProxyIfInstalled(ctx, "staging purge")

			a.log.Info("purged — the machine is deleted and its macOS disk reclaimed; active was never touched",
				"purged", machine, "active", activeMachine)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	return c
}

// printPurgeInventory shows what the confirmation is actually asking you to give
// up — how much data the staging machine holds and which snapshots go with it —
// so the decision is made against real numbers rather than the word "data".
//
// Best-effort by design: a machine that is already gone, or whose daemon does
// not answer, still has to be purgeable (that is often WHY you are purging it),
// so a failed lookup prints why and lets the prompt continue.
func (a *app) printPurgeInventory(ctx context.Context, slot, machine string) {
	ms := a.fetchStatus(ctx, slot)
	switch {
	case ms.absent:
		fmt.Printf("    (%s does not exist — there is nothing left to discard)\n\n", machine)
		return
	case ms.err != nil:
		fmt.Printf("    (%s is unreachable, so its contents cannot be listed: %s)\n\n", machine, shortErr(ms.err.Error()))
		return
	}
	fmt.Printf("    %s\n\n", dbSizeLine(a.cfg.PGDB, ms.st.DBSize))
	if t := snapshotTable(ms.st.Snapshots); t != "" {
		fmt.Printf("%s\n\n", indent(t, "    "))
		return
	}
	fmt.Printf("    (no snapshots)\n\n")
}

// stagingRebuildCmd is the hard-reset reclaim tier (spec 0002 §0.1/§2): delete
// and recreate ONLY the staging machine — reclaiming its grown sparse macOS
// disk — then re-provision a fresh backend on it. The active machine is never
// touched, so the currently-served data survives untouched throughout.
func (a *app) stagingRebuildCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rebuild",
		Short: "Hard reset: delete+recreate the staging machine to reclaim its macOS disk, then re-provision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			staging, machine, activeMachine, err := a.stagingForDestruction()
			if err != nil {
				return err
			}

			fmt.Printf("==> This DELETES %s (staging), discarding its data and snapshots, and reclaims its macOS disk.\n", machine)
			fmt.Printf("    %s (active) is never touched.\n", activeMachine)
			if !force {
				ok, err := confirm()
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("aborted")
				}
			}

			cli := a.apple(staging)
			a.log.Info("deleting and recreating the machine (this reclaims its macOS disk)", "machine", machine)
			opts := applecli.CreateOpts{CPUs: a.cfg.MachineCPUs, Memory: a.cfg.MachineMemory, Image: a.cfg.MachineImage}
			if err := cli.Recreate(ctx, opts, 5*time.Minute); err != nil {
				return err
			}

			a.log.Info("installing pgdevd", "machine", machine)
			if err := a.deploy(ctx, staging); err != nil {
				return err
			}

			a.log.Info("provisioning a fresh backend", "machine", machine)
			cl, err := a.longClientFor(ctx, staging)
			if err != nil {
				return err
			}
			if _, err := cl.Up(ctx); err != nil {
				return err
			}

			// The recreated machine has a fresh DHCP lease, so point the proxy at it.
			a.reconcileProxyIfInstalled(ctx, "staging rebuild")

			a.log.Info("reclaim done — the machine is fresh; active was never touched",
				"rebuilt", machine, "active", activeMachine)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	return c
}

// ----- shared render helpers --------------------------------------------------

func (a *app) psqlCmd(port int) string {
	return fmt.Sprintf("psql --host=%s --port=%d --username=%s --dbname=%s",
		a.cfg.ProxyHostname, port, a.cfg.PGUser, a.cfg.PGDB)
}

// snapshotsAfter returns the names created strictly after `name` (the timeline a
// restore of `name` discards), mirroring store.After. Empty if name is newest or
// absent.
func snapshotsAfter(snaps []agentapi.SnapshotInfo, name string) []string {
	var after []string
	seen := false
	for _, s := range snaps {
		if seen {
			after = append(after, s.Name)
		}
		if s.Name == name {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return after
}

// confirm prompts on a TTY; without one it refuses and tells the caller to pass
// --force (matching the shell, but now resolved host-side, not over an exec).
func confirm() (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("this confirmation needs a terminal; re-run with --force (or force=1 via make)")
	}
	fmt.Fprint(os.Stderr, "Continue? [Y/n] ")
	var reply string
	if _, err := fmt.Scanln(&reply); err != nil && err.Error() != "unexpected newline" {
		reply = "Y" // bare Enter defaults to yes, like the shell's ${reply:-Y}
	}
	reply = strings.TrimSpace(reply)
	return reply == "" || reply == "Y" || reply == "y", nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
func orAbsent(s string) string {
	if s == "" {
		return "ABSENT"
	}
	return s
}

// statusState is the STATE column for one slot. A deleted machine (never
// created, or purged) reads ABSENT — a legitimate steady state since
// `pg.staging.purge` — and is kept distinct from UNREACHABLE, which means the
// machine is there but its daemon did not answer.
func statusState(ms slotStatus) string {
	switch {
	case ms.absent:
		return "ABSENT"
	case ms.err != nil:
		return "UNREACHABLE"
	}
	return orAbsent(ms.st.State)
}

// rebuildHint names the command that recreates a deleted machine: staging has
// its own cheap rebuild (the everyday reclaim tier after `pg.staging.purge`),
// active only comes back through the full `make start` path.
func rebuildHint(role string) string {
	if role == "staging" {
		return "make pg.staging.rebuild"
	}
	return "make start"
}

// dbSizeLine reports the live database from both sides. The SQL figure is
// pg_database_size, so it counts only this database; the on-disk figure is the
// slot's whole data directory on the XFS store, which also carries WAL and the
// space PostgreSQL has not given back. The gap between them is the point of
// printing both: a store that dwarfs the database is bloat, not data.
func dbSizeLine(db string, sz agentapi.DBSize) string {
	// A daemon older than these fields answers with zeros and no error. Say that
	// once, instead of printing "0 bytes" twice as if the database were empty.
	if sz == (agentapi.DBSize{}) {
		return fmt.Sprintf("database %s: size not reported by this machine's daemon (run 'make deploy')", db)
	}
	return fmt.Sprintf("database %s: %s / %s", db,
		sizeCell(sz.SQLBytes, sz.SQLError, "(SQL: %s)", "%s (SQL)"),
		sizeCell(sz.DiskBytes, sz.DiskError, "on disk (%s)", "%s on disk"))
}

// sizeCell renders one measurement, replacing a missing number with a dash and
// the reason it is missing. Zero counts as missing: PostgreSQL never reports an
// empty database, and neither does a data directory that exists.
func sizeCell(n int64, errText, missing, present string) string {
	switch {
	case errText != "":
		return "- " + fmt.Sprintf(missing, shortErr(errText))
	case n == 0:
		return "- " + fmt.Sprintf(missing, "not reported")
	}
	return fmt.Sprintf(present, fmtBytes(n))
}

// fmtBytes renders a byte count the way pg_size_pretty does — 1024-based steps
// under the familiar kB/MB/GB labels — so the SQL figure on a status line reads
// the same as the one you would get from psql.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	v := float64(n) / unit
	exp := 0
	for v >= unit && exp < 3 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.2f %s", v, [...]string{"kB", "MB", "GB", "TB"}[exp])
}

// indent shifts every non-empty line of a block by prefix. Blank lines are left
// bare so no trailing whitespace is emitted, and the block keeps the column
// alignment tabwriter gave it.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

// shortErr trims a measurement failure to something that fits on the status
// line. The daemon sends the full message (an incus exec failure carries the
// script's whole output); the reason belongs here, the detail belongs in logs.
func shortErr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
