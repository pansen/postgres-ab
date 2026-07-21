// Package task is the write-ahead-journal transaction engine that replaces the
// hand-rolled crash-recovery state machine in scripts/pg-dev-local (the
// _snapshot / _restore signal-trap closures, the .tmp.$$/.replaced.$$ rename
// dances, and the _slot_recovery_artifact "refuse & page a human" logic).
//
// A mutating operation is expressed as an ordered list of reversible Steps plus
// a set of Ensures (postconditions enforced whether the task commits or rolls
// back). The engine journals intent before touching anything, then:
//
//   - runs each Step.Do in order, recording progress after each;
//   - on failure, decides by the Commit index whether the task is already
//     durable (roll forward the remaining Do) or not (roll back completed Undo,
//     in reverse);
//   - always runs Ensures;
//   - clears the journal record only once the task is resolved.
//
// On a fresh start Recover() replays any journal record left by a killed process
// through exactly the same resolve() path — so "crash mid-restore" auto-heals
// instead of requiring manual cleanup.
//
// CONTRACT: Step.Do, Step.Undo and Ensure.Run MUST be idempotent and convergent.
// Recovery re-runs Do (roll-forward) or Undo (roll-back) against a filesystem
// that may hold the partial effects of an interrupted step, and rollback invokes
// the in-progress step's Undo as well as those of completed steps. Each callback
// must therefore tolerate "already done" and "never done" states as no-ops.
package task

import (
	"context"
	"errors"
	"fmt"
)

// Step is one reversible unit of work.
type Step struct {
	Name string
	Do   func(ctx context.Context) error
	Undo func(ctx context.Context) error // nil = nothing to compensate
}

// Ensure is a postcondition enforced on both commit and rollback (e.g.
// "PostgreSQL is running").
type Ensure struct {
	Name string
	Run  func(ctx context.Context) error
}

// Task is a single in-flight mutation. ID must be unique per concurrently
// possible mutation (in practice: one per slot, guarded by the mutation lock).
type Task struct {
	ID      string            // journal record name, e.g. "snapshot:a"
	Kind    string            // registry key used to rebuild on Recover, e.g. "snapshot"
	Args    map[string]string // persisted verbatim; the rebuild input on Recover
	Steps   []Step
	Ensures []Ensure
	// Commit is the number of completed Steps at which the task becomes durable.
	// completed >= Commit  → roll forward the rest; completed < Commit → roll back.
	Commit int
}

func (t Task) validate() error {
	if t.ID == "" || t.Kind == "" {
		return errors.New("task: ID and Kind are required")
	}
	if t.Commit < 0 || t.Commit > len(t.Steps) {
		return fmt.Errorf("task %s: Commit %d out of range [0,%d]", t.ID, t.Commit, len(t.Steps))
	}
	return nil
}

// Run executes a fresh task, journaling as it goes.
func Run(ctx context.Context, j Journal, t Task) error {
	if err := t.validate(); err != nil {
		return err
	}
	if err := j.Begin(Record{ID: t.ID, Kind: t.Kind, Args: t.Args, Commit: t.Commit, Completed: 0}); err != nil {
		return fmt.Errorf("journal begin %s: %w", t.ID, err)
	}
	completed, stepErr := forward(ctx, j, t, 0)
	applied, resErr := resolve(ctx, j, t, completed)
	if applied {
		// Task ended in the applied state (happy path, or rolled forward past a
		// transient). Surface only errors from the resolution itself.
		return resErr
	}
	return errors.Join(stepErr, resErr)
}

// Recover resolves any journal records left behind by an interrupted process.
// It is called on startup and before any new mutation.
func Recover(ctx context.Context, j Journal, reg Registry) error {
	pending, err := j.Pending()
	if err != nil {
		return fmt.Errorf("journal pending: %w", err)
	}
	var errs []error
	for _, rec := range pending {
		build, ok := reg[rec.Kind]
		if !ok {
			errs = append(errs, fmt.Errorf("recover %s: unknown task kind %q", rec.ID, rec.Kind))
			continue
		}
		t, err := build(rec.Args)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover %s: rebuild: %w", rec.ID, err))
			continue
		}
		t.ID, t.Kind, t.Args, t.Commit = rec.ID, rec.Kind, rec.Args, rec.Commit
		if _, err := resolve(ctx, j, t, rec.Completed); err != nil {
			errs = append(errs, fmt.Errorf("recover %s: %w", rec.ID, err))
		}
	}
	return errors.Join(errs...)
}

// forward runs Step.Do from index `from`, recording progress after each success.
// It returns the number of fully-completed steps and the first error (if any).
func forward(ctx context.Context, j Journal, t Task, from int) (int, error) {
	for i := from; i < len(t.Steps); i++ {
		if err := t.Steps[i].Do(ctx); err != nil {
			return i, fmt.Errorf("step %d (%s): %w", i, t.Steps[i].Name, err)
		}
		if err := j.Advance(t.ID, i+1); err != nil {
			return i + 1, fmt.Errorf("journal advance %s: %w", t.ID, err)
		}
	}
	return len(t.Steps), nil
}

// resolve drives a task to a terminal state given how many steps completed.
// It returns whether the task ended applied, plus any error from the resolution.
func resolve(ctx context.Context, j Journal, t Task, completed int) (applied bool, err error) {
	if completed >= t.Commit {
		// Durable: finish any remaining Do (idempotent), then commit.
		if _, fErr := forward(ctx, j, t, completed); fErr != nil {
			eErr := ensures(ctx, t)
			// Leave the journal record for the next Recover to retry.
			return false, errors.Join(fErr, eErr)
		}
		eErr := ensures(ctx, t)
		if cErr := j.Commit(t.ID); cErr != nil {
			return true, errors.Join(eErr, fmt.Errorf("journal commit %s: %w", t.ID, cErr))
		}
		return true, eErr
	}
	// Not durable: roll back completed (and in-progress) steps, then commit.
	rErr := rollback(ctx, t, completed)
	eErr := ensures(ctx, t)
	if cErr := j.Commit(t.ID); cErr != nil {
		return false, errors.Join(rErr, eErr, fmt.Errorf("journal commit %s: %w", t.ID, cErr))
	}
	return false, errors.Join(rErr, eErr)
}

// rollback undoes steps from the in-progress index down to 0. Index `completed`
// (the step that failed or was interrupted) is included because its Do may have
// left partial effects; its Undo is written to converge regardless.
func rollback(ctx context.Context, t Task, completed int) error {
	var errs []error
	start := completed
	if start > len(t.Steps)-1 {
		start = len(t.Steps) - 1
	}
	for i := start; i >= 0; i-- {
		if t.Steps[i].Undo == nil {
			continue
		}
		if err := t.Steps[i].Undo(ctx); err != nil {
			errs = append(errs, fmt.Errorf("undo step %d (%s): %w", i, t.Steps[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

func ensures(ctx context.Context, t Task) error {
	var errs []error
	for _, e := range t.Ensures {
		if err := e.Run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ensure %s: %w", e.Name, err))
		}
	}
	return errors.Join(errs...)
}
