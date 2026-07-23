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

func (a *app) upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Provision both machines' backends (vpg-a, vpg-b) from empty Incus hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.cfg.RequireCreds(); err != nil {
				return err
			}
			for _, slot := range slotsAB {
				cl, err := a.longClientFor(ctx, slot)
				if err != nil {
					return err
				}
				fmt.Printf("==> [%s] Provisioning (the first run builds the golden PostgreSQL image; this can take a few minutes)...\n",
					a.cfg.MachineNameForSlot(slot))
				if _, err := cl.Up(ctx); err != nil {
					return fmt.Errorf("%s: %w", a.cfg.MachineNameForSlot(slot), err)
				}
			}
			// Default to "a" active the first time the pointer file has never
			// been written, so a fresh `pgdev up` has a well-defined role split.
			if _, err := os.Stat(a.cfg.ActiveMachinePath()); os.IsNotExist(err) {
				if err := a.active.Set("a"); err != nil {
					return err
				}
			}
			if err := a.refreshForwarder(); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: forwarder refresh: %v\n", err)
			}
			fmt.Println("==> pg-dev ready.")
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
					fmt.Fprintf(os.Stderr, "WARNING: %s: %v\n", machine, err)
					continue
				}
				res, err := cl.Down(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: %s: %v\n", machine, err)
					continue
				}
				fmt.Printf("[%s] %s\n", machine, res.Message)
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
}

// fetchStatuses queries both machines' status, tolerating per-machine errors.
func (a *app) fetchStatuses(ctx context.Context) map[string]slotStatus {
	out := make(map[string]slotStatus, len(slotsAB))
	for _, slot := range slotsAB {
		cl, err := a.clientFor(ctx, slot)
		if err != nil {
			out[slot] = slotStatus{err: err}
			continue
		}
		st, err := cl.Status(ctx)
		out[slot] = slotStatus{st: st, err: err}
	}
	return out
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
		endpoint := fmt.Sprintf("%s:%d", a.cfg.ClientHost, a.cfg.ClientPort(role))
		if ms.err != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", role, machine, "-", "UNREACHABLE", endpoint, "-", "-")
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			role, machine, orDash(ms.st.Container), orAbsent(ms.st.State), endpoint, len(ms.st.Snapshots), orDash(strings.Join(ms.st.IPs, ",")))
	}
	tw.Flush()
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
		if ms.err != nil {
			fmt.Printf("(unreachable: %v)\n\n", ms.err)
			continue
		}
		if withPsql {
			fmt.Printf("$ %s\n\n", a.psqlCmd(a.cfg.ClientPort(role)))
		}
		if len(ms.st.Snapshots) == 0 {
			fmt.Println("(no snapshots)")
		} else {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCREATED_AT")
			for _, s := range ms.st.Snapshots {
				fmt.Fprintf(tw, "%s\t%s\n", s.Name, time.Unix(s.CreatedUnix, 0).Format("2006-01-02 15:04:05 -0700"))
			}
			tw.Flush()
		}
		fmt.Println()
	}
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
			h := a.cfg.ClientHost
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
			if h == "127.0.0.1" {
				fmt.Printf("\nnote: 127.0.0.1 needs the host forwarder ('make endpoint.install'); it\n")
				fmt.Printf("      relays to each machine's IP, which may drift on reboot ('pgdev refresh' re-points it).\n")
			}
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
				a.writeMachineIPFile(slot, ip)
				endpoint := fmt.Sprintf("%s:%d", a.cfg.ClientHost, a.cfg.ClientPort(role))
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
		Short: "Flip active↔staging (re-point the host forwarder, no data moves)",
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

			if err := a.active.Set(to); err != nil {
				return err
			}
			if err := a.refreshForwarder(); err != nil {
				if rerr := a.active.Set(from); rerr != nil {
					return fmt.Errorf("forwarder refresh failed (%v) and rolling back the active pointer also failed (%v)", err, rerr)
				}
				return fmt.Errorf("forwarder refresh failed, rolled back active pointer to %s: %w", a.cfg.MachineNameForSlot(from), err)
			}

			fmt.Printf("Promoted. active=%s staging=%s\n\n",
				a.cfg.MachineNameForSlot(to), a.cfg.MachineNameForSlot(from))
			a.renderStatus(ctx)
			return nil
		},
	}
}

func (a *app) refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-discover both machine IPs and re-point the host forwarder",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			for _, slot := range slotsAB {
				machine := a.cfg.MachineNameForSlot(slot)
				ip := a.machineIP(ctx, slot)
				a.writeMachineIPFile(slot, ip)
				if ip == "" {
					fmt.Printf("[%s] no IP (machine down?) — skipping reconcile\n", machine)
					continue
				}
				cl, err := a.clientFor(ctx, slot)
				if err != nil {
					fmt.Printf("[%s] %v\n", machine, err)
					continue
				}
				res, err := cl.Reconcile(ctx)
				if err != nil {
					fmt.Printf("[%s] reconcile: %v\n", machine, err)
					continue
				}
				fmt.Printf("[%s] backend running=%v\n", machine, res.BackendRunning)
				for _, act := range res.Actions {
					fmt.Printf("    %s\n", act)
				}
			}
			if err := a.refreshForwarder(); err != nil {
				return fmt.Errorf("forwarder refresh: %w", err)
			}
			fmt.Printf("Re-pointed endpoints: active %s:%d → %s, staging %s:%d → %s\n",
				a.cfg.ClientHost, a.cfg.ClientActivePort, a.cfg.MachineNameForSlot(a.active.Get()),
				a.cfg.ClientHost, a.cfg.ClientStagingPort, a.cfg.MachineNameForSlot(a.active.Staging()))
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
			cl, err := a.clientForRole(ctx, role)
			if err != nil {
				return err
			}
			res, err := cl.Snapshot(ctx, agentapi.SnapshotRequest{Name: args[0], Force: force})
			if err != nil {
				return err
			}
			fmt.Println(res.Message)
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
		fmt.Printf("==> Restoring %s to most recent snapshot: %s\n", machine, target)
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

	res, err := cl.Restore(ctx, agentapi.RestoreRequest{Name: name, Last: last, Force: effForce})
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
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
			cl, err := a.longClientForRole(ctx, "staging")
			if err != nil {
				return err
			}
			res, err := cl.Start(ctx)
			if err != nil {
				return err
			}
			fmt.Println(res.Message)
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
			fmt.Println(res.Message)
			return nil
		},
	}
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
			staging := a.active.Staging()
			active := a.active.Get()
			machine := a.cfg.MachineNameForSlot(staging)
			activeMachine := a.cfg.MachineNameForSlot(active)
			// Structurally staging != active (Staging() is the pointer's
			// complement), but this is the load-bearing safety property of the
			// whole command, so assert it explicitly rather than trust that.
			if machine == activeMachine {
				return fmt.Errorf("refusing to rebuild %s — it is the active machine", machine)
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
			fmt.Printf("==> [%s] Deleting and recreating (this reclaims its macOS disk)...\n", machine)
			opts := applecli.CreateOpts{CPUs: a.cfg.MachineCPUs, Memory: a.cfg.MachineMemory, Image: a.cfg.MachineImage}
			if err := cli.Recreate(ctx, opts, 5*time.Minute); err != nil {
				return err
			}

			fmt.Printf("==> [%s] Installing pgdevd...\n", machine)
			if err := a.deploy(ctx, staging); err != nil {
				return err
			}

			fmt.Printf("==> [%s] Provisioning fresh backend...\n", machine)
			cl, err := a.longClientFor(ctx, staging)
			if err != nil {
				return err
			}
			if _, err := cl.Up(ctx); err != nil {
				return err
			}

			if err := a.refreshForwarder(); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: forwarder refresh: %v\n", err)
			}

			fmt.Printf("==> Reclaim done. %s is fresh; %s (active) was never touched.\n", machine, activeMachine)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	return c
}

// ----- shared render helpers --------------------------------------------------

func (a *app) psqlCmd(port int) string {
	return fmt.Sprintf("psql --host=%s --port=%d --username=%s --dbname=%s",
		a.cfg.ClientHost, port, a.cfg.PGUser, a.cfg.PGDB)
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
func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
