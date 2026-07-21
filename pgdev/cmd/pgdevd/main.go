// Command pgdevd is the in-machine agent for the snapshottable-PostgreSQL setup.
//
// In Slice 1 it is a one-shot CLI, invoked by scripts/pg-dev-local for the
// snapshot/restore command family: it runs each mutation through the journaled
// task engine (internal/task), auto-healing any interrupted prior run first.
// Later slices grow a `serve` subcommand (the resident HTTP daemon) on the same
// binary; the cobra command surface and the typed Incus client land with them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"pansen.me/pgdev/internal/backend"
	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/logx"
	"pansen.me/pgdev/internal/ops"
	"pansen.me/pgdev/internal/store"
	"pansen.me/pgdev/internal/task"
)

// version is stamped at build time (see Makefile).
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "snapshot":
		return cmdSnapshot(ctx, args[1:])
	case "restore":
		return cmdRestore(ctx, args[1:], false)
	case "restore-last":
		return cmdRestore(ctx, args[1:], true)
	case "start":
		return cmdStart(ctx, args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// env wires up the shared dependencies and runs recovery for any interrupted
// prior mutation before returning.
func env(ctx context.Context) (*ops.Ops, task.Journal, error) {
	cfg := config.FromEnv()
	st := store.NewOSStore(cfg.DataRoot)
	if err := st.RequireMounted(); err != nil {
		return nil, nil, err
	}
	j, err := task.NewFileJournal(st.JournalDir())
	if err != nil {
		return nil, nil, err
	}
	be := backend.NewIncus(cfg)
	be.Log = logx.Stderr
	o := ops.New(st, be, cfg)
	o.Log = logx.Stderr
	if err := task.Recover(ctx, j, o.Registry()); err != nil {
		return nil, nil, fmt.Errorf("recovering interrupted operation: %w", err)
	}
	return o, j, nil
}

// cmdStart brings a backend fully up: container running AND PostgreSQL ready.
// It needs neither the journal nor the data store, so it skips env().
func cmdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	slot := fs.String("slot", "", "backend slot (a|b)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSlot(*slot); err != nil {
		return err
	}
	cfg := config.FromEnv()
	be := backend.NewIncus(cfg)
	be.Log = logx.Stderr
	o := ops.New(store.NewOSStore(cfg.DataRoot), be, cfg)
	o.Log = logx.Stderr
	c := o.Cfg.Container(*slot)
	if err := o.Backend.StartContainerAndWait(ctx, c); err != nil {
		return err
	}
	fmt.Printf("Started %s (PostgreSQL ready).\n", c)
	return nil
}

func cmdSnapshot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	slot := fs.String("slot", "", "backend slot (a|b)")
	name := fs.String("name", "", "snapshot name")
	force := fs.Bool("force", false, "replace an existing same-named snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSlot(*slot); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	o, j, err := env(ctx)
	if err != nil {
		return err
	}
	t, err := o.Snapshot(ctx, *slot, *name, *force)
	if err != nil {
		return err
	}
	if err := task.Run(ctx, j, t); err != nil {
		return err
	}
	fmt.Printf("Snapshot %q created on %s.\n", *name, o.Cfg.Container(*slot))
	return nil
}

func cmdRestore(ctx context.Context, args []string, last bool) error {
	name := "restore"
	if last {
		name = "restore-last"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	slot := fs.String("slot", "", "backend slot (a|b)")
	snap := fs.String("name", "", "snapshot name (ignored with restore-last)")
	force := fs.Bool("force", false, "delete newer snapshots without confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireSlot(*slot); err != nil {
		return err
	}
	o, j, err := env(ctx)
	if err != nil {
		return err
	}

	target := *snap
	if last {
		if target, err = o.Store.Last(*slot); err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("no snapshots on %s", o.Cfg.Container(*slot))
		}
		fmt.Printf("==> Restoring %s to most recent snapshot: %s\n", o.Cfg.Container(*slot), target)
	}
	if target == "" {
		return errors.New("--name is required")
	}

	// Preserve the shell's timeline semantics: restoring an older checkpoint
	// discards every later one, after confirmation.
	after, err := o.After(*slot, target)
	if err != nil {
		return err
	}
	if len(after) > 0 {
		fmt.Printf("==> Snapshots on %s that will be deleted:\n     %s\n",
			o.Cfg.Container(*slot), strings.Join(after, "\n     "))
		if !*force {
			ok, err := confirm()
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("aborted")
			}
		}
	}

	t, err := o.Restore(ctx, *slot, target)
	if err != nil {
		return err
	}
	if err := task.Run(ctx, j, t); err != nil {
		return err
	}
	fmt.Printf("Restored %s to snapshot %q.\n", o.Cfg.Container(*slot), target)
	return nil
}

// confirm prompts on a TTY; through the non-TTY machine transport it refuses
// (stdin delivers EOF immediately), matching the shell and telling the caller to
// pass --force.
func confirm() (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("this confirmation needs a terminal; re-run with force=1 (make) or --force")
	}
	fmt.Fprint(os.Stderr, "Continue? [Y/n] ")
	var reply string
	if _, err := fmt.Scanln(&reply); err != nil && err.Error() != "unexpected newline" {
		// A bare Enter (EOF/newline) defaults to yes, like the shell's ${reply:-Y}.
		reply = "Y"
	}
	reply = strings.TrimSpace(reply)
	return reply == "" || reply == "Y" || reply == "y", nil
}

func requireSlot(slot string) error {
	if slot != "a" && slot != "b" {
		return fmt.Errorf("--slot must be a or b (got %q)", slot)
	}
	return nil
}

func usage() error {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
pgdevd — in-machine agent (Slice 1: snapshot/restore engine)

Usage:
  pgdevd snapshot     --slot <a|b> --name <name> [--force]
  pgdevd restore      --slot <a|b> --name <name> [--force]
  pgdevd restore-last --slot <a|b> [--force]
  pgdevd start        --slot <a|b>
  pgdevd version
`))
	return nil
}
