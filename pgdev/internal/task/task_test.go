package task

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// recorder builds tasks whose steps append their names to a log, so tests can
// assert the exact Do/Undo/Ensure sequence.
type recorder struct {
	log      []string
	failAt   int  // index of the step whose Do fails (-1 = none)
	failOnce bool // if set, the failing step succeeds on a second Do (roll-forward)
	failed   bool
}

func (r *recorder) step(i int, name string) Step {
	return Step{
		Name: name,
		Do: func(context.Context) error {
			if i == r.failAt && !(r.failOnce && r.failed) {
				r.failed = true
				r.log = append(r.log, "do:"+name+":FAIL")
				return errors.New("boom at " + name)
			}
			r.log = append(r.log, "do:"+name)
			return nil
		},
		Undo: func(context.Context) error {
			r.log = append(r.log, "undo:"+name)
			return nil
		},
	}
}

func (r *recorder) task(commit int) Task {
	return Task{
		ID:   "t:1",
		Kind: "model",
		Steps: []Step{
			r.step(0, "s0"),
			r.step(1, "s1"),
			r.step(2, "s2"),
		},
		Ensures: []Ensure{{Name: "ens", Run: func(context.Context) error {
			r.log = append(r.log, "ensure")
			return nil
		}}},
		Commit: commit,
	}
}

func newJournal(t *testing.T) *FileJournal {
	t.Helper()
	j, err := NewFileJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func assertNoPending(t *testing.T, j *FileJournal) {
	t.Helper()
	p, err := j.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 0 {
		t.Fatalf("journal not cleared: %+v", p)
	}
}

func TestRunHappyPath(t *testing.T) {
	r := &recorder{failAt: -1}
	j := newJournal(t)
	if err := Run(context.Background(), j, r.task(3)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(r.log, ",")
	want := "do:s0,do:s1,do:s2,ensure"
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	assertNoPending(t, j)
}

func TestRunRollbackBeforeCommit(t *testing.T) {
	r := &recorder{failAt: 1} // s1 fails; commit=3 so completed(1) < commit → roll back
	j := newJournal(t)
	err := Run(context.Background(), j, r.task(3))
	if err == nil {
		t.Fatal("expected error")
	}
	got := strings.Join(r.log, ",")
	// s0 done, s1 fails, then undo s1 (in-progress) and s0, then ensure.
	want := "do:s0,do:s1:FAIL,undo:s1,undo:s0,ensure"
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	assertNoPending(t, j)
}

func TestRunRollForwardPastCommit(t *testing.T) {
	// commit=1: once s0 completes the task is durable. s1 fails the first time
	// but succeeds on the roll-forward retry, so the task applies.
	r := &recorder{failAt: 1, failOnce: true}
	j := newJournal(t)
	if err := Run(context.Background(), j, r.task(1)); err != nil {
		t.Fatalf("Run should have rolled forward: %v", err)
	}
	got := strings.Join(r.log, ",")
	want := "do:s0,do:s1:FAIL,do:s1,do:s2,ensure"
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	assertNoPending(t, j)
}

// registry rebuilds a recorder task for recovery tests.
func regFor(r *recorder, commit int) Registry {
	return Registry{"model": func(map[string]string) (Task, error) { return r.task(commit), nil }}
}

func TestRecoverRollsBack(t *testing.T) {
	j := newJournal(t)
	// Simulate a process that completed s0 then died (completed=1 < commit=3).
	if err := j.Begin(Record{ID: "t:1", Kind: "model", Commit: 3, Completed: 1}); err != nil {
		t.Fatal(err)
	}
	r := &recorder{failAt: -1}
	if err := Recover(context.Background(), j, regFor(r, 3)); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := strings.Join(r.log, ",")
	// rollback undoes the in-progress step (index1) and completed step0.
	want := "undo:s1,undo:s0,ensure"
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	assertNoPending(t, j)
}

func TestRecoverRollsForward(t *testing.T) {
	j := newJournal(t)
	// Completed past the commit point (completed=2 >= commit=1): finish the rest.
	if err := j.Begin(Record{ID: "t:1", Kind: "model", Commit: 1, Completed: 2}); err != nil {
		t.Fatal(err)
	}
	r := &recorder{failAt: -1}
	if err := Recover(context.Background(), j, regFor(r, 1)); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := strings.Join(r.log, ",")
	want := "do:s2,ensure" // only the remaining step, then ensure
	if got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	assertNoPending(t, j)
}

func TestValidateRejectsBadCommit(t *testing.T) {
	j := newJournal(t)
	err := Run(context.Background(), j, Task{ID: "x", Kind: "k", Commit: 5})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestFileJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Begin(Record{ID: "a:1", Kind: "snapshot", Args: map[string]string{"slot": "a"}, Commit: 2}); err != nil {
		t.Fatal(err)
	}
	if err := j.Advance("a:1", 1); err != nil {
		t.Fatal(err)
	}
	// The colon in the ID must not leak into the filename.
	if _, err := os.Stat(dir + "/a_1.json"); err != nil {
		t.Fatalf("expected escaped filename: %v", err)
	}
	p, err := j.Pending()
	if err != nil || len(p) != 1 || p[0].Completed != 1 || p[0].Args["slot"] != "a" {
		t.Fatalf("Pending = %+v, err %v", p, err)
	}
	if err := j.Commit("a:1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := j.Pending(); len(p) != 0 {
		t.Fatalf("record not removed: %+v", p)
	}
}
