package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/agentapi"
)

// ----- lifecycle (up / down) -----------------------------------------------

func (a *app) upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Provision pg-dev-a, pg-dev-b and pg-proxy from an empty Incus host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.cfg.RequireCreds(); err != nil {
				return err
			}
			cl, err := a.longClient(ctx)
			if err != nil {
				return err
			}
			fmt.Println("==> Provisioning (the first run builds the golden PostgreSQL image; this can take a few minutes)...")
			st, err := cl.Up(ctx)
			if err != nil {
				return err
			}
			fmt.Println("==> pg-dev ready.")
			fmt.Println()
			a.renderStatus(ctx, st)
			return nil
		},
	}
}

func (a *app) downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Delete the containers and both XFS data trees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.longClient(ctx)
			if err != nil {
				return err
			}
			res, err := cl.Down(ctx)
			if err != nil {
				return err
			}
			fmt.Println(res.Message)
			return nil
		},
	}
}

// ----- status --------------------------------------------------------------

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Pointer, proxy roles, per-backend state/endpoints, and snapshot timelines",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			st, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			a.renderStatus(ctx, st)
			return nil
		},
	}
}

func (a *app) renderStatus(ctx context.Context, st agentapi.StatusResponse) {
	if st.IncusVersion != "" {
		fmt.Println(st.IncusVersion)
		fmt.Println()
	}
	active := st.Active
	staging := "b"
	if active == "b" {
		staging = "a"
	}
	fmt.Printf("active slot: %s   (active = %s, staging = %s)\n",
		active, a.cfg.Container(active), a.cfg.Container(staging))
	machineIP := a.machineIP(ctx)
	if st.ProxyState == "RUNNING" {
		fmt.Printf("pg-proxy:    RUNNING  (machine %s → :%d active, :%d staging)\n",
			orQ(machineIP), a.cfg.ActivePort, a.cfg.StagingPort)
	} else {
		fmt.Printf("pg-proxy:    %s\n", orAbsent(st.ProxyState))
	}
	if !st.DataStoreMounted {
		fmt.Println("data store:  NOT MOUNTED — run 'make machine'")
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tENDPOINT\tSNAPSHOTS\tIPs")
	for _, b := range st.Backends {
		endpoint := fmt.Sprintf("%s:%d", a.cfg.ClientHost, a.cfg.ClientPort(b.Role))
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			b.Container, orDash(b.State), endpoint, len(b.Snapshots), orDash(strings.Join(b.IPs, ",")))
	}
	tw.Flush()
	fmt.Println()
	a.renderSnapshots(st, true)
}

// ----- snapshots -----------------------------------------------------------

func (a *app) snapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots",
		Short: "Snapshot timelines for active and staging",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			st, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			a.renderSnapshots(st, false)
			return nil
		},
	}
}

func (a *app) renderSnapshots(st agentapi.StatusResponse, withPsql bool) {
	for _, b := range st.Backends {
		fmt.Printf("─── %-7s (%s) ───\n", b.Role, b.Container)
		if withPsql {
			fmt.Printf("$ %s\n\n", a.psqlCmd(a.cfg.ClientPort(b.Role)))
		}
		if len(b.Snapshots) == 0 {
			fmt.Println("(no snapshots)")
		} else {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCREATED_AT")
			for _, s := range b.Snapshots {
				fmt.Fprintf(tw, "%s\t%s\n", s.Name, time.Unix(s.CreatedUnix, 0).Format("2006-01-02 15:04:05 -0700"))
			}
			tw.Flush()
		}
		fmt.Println()
	}
}

// ----- endpoint & ip -------------------------------------------------------

func (a *app) endpointCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "endpoint",
		Short: "Print client endpoints, .pgpass lines, and psql commands",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := a.cfg.RequireCreds(); err != nil {
				return err
			}
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			st, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			if st.ProxyState != "RUNNING" {
				return fmt.Errorf("%s is not running — 'make start' (then 'make pg.up' if never provisioned)", a.cfg.ProxyName)
			}
			h := a.cfg.ClientHost
			fmt.Printf("active   host=%s port=%d dbname=%s   (current data)\n", h, a.cfg.ClientActivePort, a.cfg.PGDB)
			fmt.Printf("staging  host=%s port=%d dbname=%s   (import target / opposite of active)\n\n", h, a.cfg.ClientStagingPort, a.cfg.PGDB)
			fmt.Println(".pgpass lines:")
			fmt.Printf("%s:%d:*:%s:%s\n", h, a.cfg.ClientActivePort, a.cfg.PGUser, a.cfg.PGPassword)
			fmt.Printf("%s:%d:*:%s:%s\n\n", h, a.cfg.ClientStagingPort, a.cfg.PGUser, a.cfg.PGPassword)
			fmt.Println("psql commands:")
			fmt.Printf("  active:  %s\n", a.psqlCmd(a.cfg.ClientActivePort))
			fmt.Printf("  staging: %s\n", a.psqlCmd(a.cfg.ClientStagingPort))
			if h == "127.0.0.1" {
				fmt.Printf("\nnote: 127.0.0.1 needs the host forwarder ('make endpoint.install'); it\n")
				fmt.Printf("      relays to the machine IP %s, which may drift on reboot.\n", orQ(a.machineIP(ctx)))
			}
			return nil
		},
	}
}

func (a *app) ipCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ip",
		Short: "Machine IP, client endpoint, and per-slot backend addresses",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			machineIP := a.machineIP(ctx)
			a.writeMachineIPFile(machineIP)
			fmt.Printf("machine IP:      %s   (drifts across reboots; forwarder target)\n", orQ(machineIP))
			fmt.Printf("client endpoint: %s   (stable; via 'make endpoint.install')\n\n", a.cfg.ClientHost)

			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			st, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			for _, b := range st.Backends {
				endpoint := fmt.Sprintf("%s:%d", a.cfg.ClientHost, a.cfg.ClientPort(b.Role))
				info := orAbsent(b.State)
				if len(b.IPs) > 0 {
					info = b.IPs[0]
				}
				fmt.Fprintf(tw, "%s\t%s\t→\t%s (%s)\n", b.Role, endpoint, b.Container, info)
			}
			tw.Flush()
			return nil
		},
	}
}

// ----- promote & refresh ---------------------------------------------------

func (a *app) promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote",
		Short: "Flip active↔staging (re-point both forwards)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			res, err := cl.Promote(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("==> Promoted. Active: %s   Staging: %s\n\n",
				a.cfg.Container(res.To), a.cfg.Container(res.From))
			a.renderStatus(ctx, res.Status)
			return nil
		},
	}
}

func (a *app) refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-pin backend IPs and re-assert the proxy forwards",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			a.writeMachineIPFile(a.machineIP(ctx))
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			res, err := cl.Reconcile(ctx)
			if err != nil {
				return err
			}
			if !res.ProxyRunning {
				fmt.Printf("Proxy %s not running; forwards left untouched.\n", a.cfg.ProxyName)
				return nil
			}
			fmt.Printf("Refreshed forwards (:%d → active, :%d → staging).\n", a.cfg.ActivePort, a.cfg.StagingPort)
			for _, act := range res.Actions {
				fmt.Printf("  %s\n", act)
			}
			return nil
		},
	}
}

// ----- snapshot / restore (active + staging) -------------------------------

func (a *app) snapshotCmd(role string) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "snapshot <name>",
		Short: "Create an XFS reflink snapshot on the " + role + " backend",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			res, err := cl.Snapshot(ctx, agentapi.SnapshotRequest{Slot: a.slot(role), Name: args[0], Force: force})
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
	slot := a.slot(role)
	cl, err := a.client(ctx)
	if err != nil {
		return err
	}
	snaps, err := cl.Snapshots(ctx, slot)
	if err != nil {
		return err
	}
	target := name
	if last {
		if len(snaps.Snapshots) == 0 {
			return fmt.Errorf("no snapshots on %s", a.cfg.Container(slot))
		}
		target = snaps.Snapshots[len(snaps.Snapshots)-1].Name
		fmt.Printf("==> Restoring %s to most recent snapshot: %s\n", a.cfg.Container(slot), target)
	}

	after := snapshotsAfter(snaps.Snapshots, target)
	effForce := force
	if len(after) > 0 {
		fmt.Printf("==> Snapshots on %s that will be deleted:\n     %s\n",
			a.cfg.Container(slot), strings.Join(after, "\n     "))
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

	res, err := cl.Restore(ctx, agentapi.RestoreRequest{Slot: slot, Name: name, Last: last, Force: effForce})
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	return nil
}

// ----- staging group -------------------------------------------------------

func (a *app) stagingCmd() *cobra.Command {
	c := &cobra.Command{Use: "staging", Short: "Operate on the staging (non-active) backend"}
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Restore staging to its 'initial' snapshot",
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
			cl, err := a.longClient(ctx)
			if err != nil {
				return err
			}
			res, err := cl.StartStaging(ctx)
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
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			res, err := cl.StopStaging(ctx)
			if err != nil {
				return err
			}
			fmt.Println(res.Message)
			return nil
		},
	}
}

// ----- shared render helpers -----------------------------------------------

func (a *app) psqlCmd(port int) string {
	return fmt.Sprintf("psql --host=%s --port=%d --username=%s --dbname=%s",
		a.cfg.ClientHost, port, a.cfg.PGUser, a.cfg.PGDB)
}

func (a *app) writeMachineIPFile(ip string) {
	if ip == "" {
		return
	}
	path := a.machineIPFile()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(ip+"\n"), 0o644)
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
