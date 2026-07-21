// Package ops assembles snapshot/restore/restore-last into task.Tasks over the
// store and backend. It is the worked example from spec §5.5.2: the 122-line
// _snapshot and 255-line _restore shell functions become linear lists of
// reversible steps; the task engine owns journaling, rollback and recovery.
//
// Each operation has two entry points sharing one assembly function:
//   - a live constructor (Snapshot/Restore/RestoreLast) that inspects the world,
//     runs prechecks, and records the decisions it made into the task Args;
//   - a Registry builder that rebuilds the identical task from those Args during
//     crash recovery — so recovery uses the SAME captured plan (e.g. the exact
//     set of newer snapshots to prune), never a recomputed one.
package ops

import (
	"context"
	"fmt"
	"strings"

	"pansen.me/pgdev/internal/backend"
	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/logx"
	"pansen.me/pgdev/internal/store"
	"pansen.me/pgdev/internal/task"
)

type Ops struct {
	Store   *store.Store
	Backend backend.Backend
	Cfg     config.Config
	Log     logx.Func // progress logging (nil = silent); stamped onto built tasks
}

func New(st *store.Store, be backend.Backend, cfg config.Config) *Ops {
	return &Ops{Store: st, Backend: be, Cfg: cfg}
}

// Registry returns the recovery builders keyed by task kind.
func (o *Ops) Registry() task.Registry {
	return task.Registry{
		"snapshot": func(a map[string]string) (task.Task, error) {
			return o.buildSnapshot(a["slot"], a["name"], a["replaced"] == "1"), nil
		},
		"restore": func(a map[string]string) (task.Task, error) {
			return o.buildRestore(a["slot"], a["name"], a["wasRunning"] == "1", splitList(a["after"])), nil
		},
	}
}

// ----- snapshot ------------------------------------------------------------

// Snapshot builds a live snapshot task after prechecks. force replaces an
// existing same-named snapshot.
func (o *Ops) Snapshot(ctx context.Context, slot, name string, force bool) (task.Task, error) {
	if err := store.ValidateName(name); err != nil {
		return task.Task{}, err
	}
	if err := o.Store.EnsureLayout(slot); err != nil {
		return task.Task{}, err
	}
	c := o.Cfg.Container(slot)

	target := o.Store.Snapshot(slot, name)
	replaced := store.Exists(target)
	if replaced && !force {
		return task.Task{}, fmt.Errorf("snapshot %q already exists on %s (pass --force to replace it)", name, c)
	}

	running, err := o.Backend.ContainerRunning(ctx, c)
	if err != nil {
		return task.Task{}, err
	}
	if !running {
		return task.Task{}, fmt.Errorf("%s is not running; start it first (e.g. make pg.staging.start)", c)
	}
	// Automate the up-check: if the container is running but PostgreSQL is down
	// (e.g. a container boot that didn't start the unit), bring it up and wait —
	// a snapshot should never fail merely because PG needs a nudge.
	if err := o.Backend.EnsurePGRunning(ctx, c); err != nil {
		return task.Task{}, err
	}

	t := o.buildSnapshot(slot, name, replaced)
	t.Args = map[string]string{"slot": slot, "name": name, "replaced": boolStr(replaced)}
	return t, nil
}

func (o *Ops) buildSnapshot(slot, name string, replaced bool) task.Task {
	c := o.Cfg.Container(slot)
	current := o.Store.Current(slot)
	target := o.Store.Snapshot(slot, name)
	staging := o.Store.Snapshot(slot, ".stage."+name)
	replacedDir := o.Store.Snapshot(slot, ".replaced."+name)

	steps := []task.Step{
		{
			Name: "stop-postgres", // snapshot a quiesced data dir
			Do:   func(ctx context.Context) error { return o.Backend.StopPG(ctx, c) },
			// Undo is nil: the "postgres-running" Ensure restarts PG on every exit.
		},
		{
			Name: "stage-reflink-clone",
			Do:   func(ctx context.Context) error { return o.Store.StageClone(ctx, current, staging) },
			Undo: func(ctx context.Context) error { return store.RemoveAll(staging) },
		},
	}
	if replaced {
		steps = append(steps, task.Step{
			Name: "aside-existing",
			Do:   func(ctx context.Context) error { return store.Move(target, replacedDir) },
			Undo: func(ctx context.Context) error { return store.Move(replacedDir, target) },
		})
	}
	steps = append(steps, task.Step{
		Name: "install",
		Do:   func(ctx context.Context) error { return store.Move(staging, target) },
		Undo: func(ctx context.Context) error { return store.Move(target, staging) },
	})
	commit := len(steps) // durable once "install" completes
	if replaced {
		steps = append(steps, task.Step{
			Name: "drop-replaced",
			Do:   func(ctx context.Context) error { return store.RemoveAll(replacedDir) },
		})
	}

	return task.Task{
		ID:      "snapshot:" + slot,
		Kind:    "snapshot",
		Steps:   steps,
		Commit:  commit,
		Ensures: []task.Ensure{{Name: "postgres-running", Run: func(ctx context.Context) error { return o.Backend.EnsurePGRunning(ctx, c) }}},
		Log:     o.Log,
	}
}

// ----- restore -------------------------------------------------------------

// After returns the snapshots newer than `name` on `slot` (those a restore would
// discard). Callers use it to confirm before mutating.
func (o *Ops) After(slot, name string) ([]string, error) { return o.Store.After(slot, name) }

// Restore builds a live restore task after prechecks. Confirmation of the
// discarded timeline is the caller's responsibility (see After); `force` here
// only records that the caller resolved it.
func (o *Ops) Restore(ctx context.Context, slot, name string) (task.Task, error) {
	if err := store.ValidateName(name); err != nil {
		return task.Task{}, err
	}
	if !store.Exists(o.Store.Snapshot(slot, name)) {
		return task.Task{}, fmt.Errorf("snapshot %q does not exist on %s", name, o.Cfg.Container(slot))
	}
	after, err := o.Store.After(slot, name)
	if err != nil {
		return task.Task{}, err
	}
	c := o.Cfg.Container(slot)
	wasRunning, err := o.Backend.ContainerRunning(ctx, c)
	if err != nil {
		return task.Task{}, err
	}
	t := o.buildRestore(slot, name, wasRunning, after)
	t.Args = map[string]string{"slot": slot, "name": name, "wasRunning": boolStr(wasRunning), "after": strings.Join(after, ",")}
	return t, nil
}

// RestoreLast restores the most recent snapshot on a slot.
func (o *Ops) RestoreLast(ctx context.Context, slot string) (task.Task, error) {
	name, err := o.Store.Last(slot)
	if err != nil {
		return task.Task{}, err
	}
	if name == "" {
		return task.Task{}, fmt.Errorf("no snapshots on %s", o.Cfg.Container(slot))
	}
	return o.Restore(ctx, slot, name)
}

func (o *Ops) buildRestore(slot, name string, wasRunning bool, after []string) task.Task {
	c := o.Cfg.Container(slot)
	current := o.Store.Current(slot)
	target := o.Store.Snapshot(slot, name)
	newDir := current + ".restore-new"
	oldDir := current + ".restore-old"
	pruneDir := o.Store.Snapshot(slot, ".restore-prune")

	steps := []task.Step{
		{
			Name: "stage-reflink-clone", // build the candidate while current still serves
			Do:   func(ctx context.Context) error { return o.Store.StageClone(ctx, target, newDir) },
			Undo: func(ctx context.Context) error { return store.RemoveAll(newDir) },
		},
		{
			Name: "stop-container",
			Do:   func(ctx context.Context) error { return o.Backend.StopContainer(ctx, c) },
			// Undo nil: the container-running Ensure restarts it if it was running.
		},
		{
			Name: "repair-ip", // surface a networking fault before touching data
			Do:   func(ctx context.Context) error { return o.Backend.RepairIP(ctx, c) },
		},
		{
			Name: "swap-data",
			Do: func(ctx context.Context) error {
				if err := store.Move(current, oldDir); err != nil {
					return err
				}
				return store.Move(newDir, current)
			},
			Undo: func(ctx context.Context) error {
				if !store.Exists(oldDir) {
					return nil // swap never ran
				}
				if err := o.Backend.StopContainerForce(ctx, c); err != nil {
					return err // PG must not hold the mount while we move data back
				}
				if err := store.Move(current, newDir); err != nil {
					return err
				}
				return store.Move(oldDir, current)
			},
		},
		{
			Name: "prune-newer-timeline",
			Do:   func(ctx context.Context) error { return o.prune(pruneDir, slot, after) },
			Undo: func(ctx context.Context) error { return o.unprune(pruneDir, slot, after) },
		},
		{
			Name: "start-container",
			Do:   func(ctx context.Context) error { return o.Backend.StartContainerAndWait(ctx, c) },
			Undo: func(ctx context.Context) error { return o.Backend.StopContainerForce(ctx, c) },
		},
	}
	commit := len(steps) // durable once the restored data has started and the timeline is pruned
	steps = append(steps, task.Step{
		Name: "commit-cleanup",
		Do: func(ctx context.Context) error {
			if err := store.RemoveAll(oldDir); err != nil {
				return err
			}
			return store.RemoveAll(pruneDir)
		},
	})

	var ensures []task.Ensure
	if wasRunning {
		ensures = append(ensures, task.Ensure{
			Name: "container-running",
			Run:  func(ctx context.Context) error { return o.Backend.StartContainerAndWait(ctx, c) },
		})
	}

	return task.Task{
		ID:      "restore:" + slot,
		Kind:    "restore",
		Steps:   steps,
		Commit:  commit,
		Ensures: ensures,
		Log:     o.Log,
	}
}

// prune moves each newer snapshot aside into pruneDir (reversibly).
func (o *Ops) prune(pruneDir, slot string, after []string) error {
	if len(after) == 0 {
		return nil
	}
	if err := ensureDir(pruneDir); err != nil {
		return err
	}
	for _, n := range after {
		if err := store.Move(o.Store.Snapshot(slot, n), pruneAt(pruneDir, n)); err != nil {
			return err
		}
	}
	return nil
}

// unprune restores snapshots moved aside by prune and removes the staging dir.
func (o *Ops) unprune(pruneDir, slot string, after []string) error {
	if !store.Exists(pruneDir) {
		return nil
	}
	for _, n := range after {
		if err := store.Move(pruneAt(pruneDir, n), o.Store.Snapshot(slot, n)); err != nil {
			return err
		}
	}
	return store.RemoveAll(pruneDir)
}
