package applecli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fake records every `container` invocation and returns programmed responses.
// resp is keyed by the first word after the verb pair we care about ("machine
// create", "machine stop", ...) joined by spaces; a per-key call counter lets a
// test return different results across repeated calls (e.g. inspect before/after
// a delete).
type fake struct {
	calls   [][]string
	handler func(n int, args []string) (string, error)
	seen    map[string]int // key -> times called
}

func (f *fake) run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	key := strings.Join(args, " ")
	n := f.seen[key]
	f.seen[key]++
	return f.handler(n, args)
}

func newCLI(f *fake) *CLI {
	return &CLI{Machine: "vpg-a", run: f.run, pollInterval: time.Millisecond}
}

func (f *fake) called(sub string) bool {
	for _, a := range f.calls {
		if strings.Contains(strings.Join(a, " "), sub) {
			return true
		}
	}
	return false
}

func TestCreatePassesResources(t *testing.T) {
	f := &fake{handler: func(int, []string) (string, error) { return "", nil }}
	c := newCLI(f)
	if err := c.Create(context.Background(), CreateOpts{CPUs: 4, Memory: "8G", Image: "local/img:26.04"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	for _, want := range []string{"machine create", "--name vpg-a", "--cpus 4", "--memory 8G", "--home-mount rw", "local/img:26.04"} {
		if !strings.Contains(got, want) {
			t.Errorf("create args missing %q in %q", want, got)
		}
	}
	if f.called("--set-default") {
		t.Error("create must NOT set-default with two machines")
	}
}

func TestDeleteVerifiesGone(t *testing.T) {
	// inspect: exists first (nil), gone after (err). stop/delete tolerated.
	f := &fake{handler: func(n int, args []string) (string, error) {
		if args[1] == "inspect" {
			if n == 0 {
				return "", nil // exists
			}
			return "", errors.New("not found") // gone after delete
		}
		if args[1] == "delete" {
			return "", errors.New("XPC timeout") // completes despite error
		}
		return "", nil
	}}
	c := newCLI(f)
	if err := c.Delete(context.Background()); err != nil {
		t.Fatalf("Delete should tolerate the delete error and verify gone: %v", err)
	}
	if !f.called("machine stop vpg-a") {
		t.Error("Delete should stop before delete")
	}
	if !f.called("machine delete vpg-a") {
		t.Error("Delete should call delete")
	}
}

func TestDeleteStillPresentErrors(t *testing.T) {
	f := &fake{handler: func(int, []string) (string, error) { return "", nil }} // inspect always succeeds → never gone
	c := newCLI(f)
	err := c.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("want 'still present' error, got %v", err)
	}
}

func TestDeleteNoopWhenAbsent(t *testing.T) {
	f := &fake{handler: func(n int, args []string) (string, error) {
		if args[1] == "inspect" {
			return "", errors.New("not found")
		}
		t.Fatalf("must not touch a machine that does not exist: %v", args)
		return "", nil
	}}
	c := newCLI(f)
	if err := c.Delete(context.Background()); err != nil {
		t.Fatalf("Delete of absent machine should be a no-op: %v", err)
	}
}

func TestBootRetriesUntilExecableThenReady(t *testing.T) {
	trueCalls := 0
	f := &fake{handler: func(n int, args []string) (string, error) {
		// `run --root -- true`
		if last(args) == "true" {
			trueCalls++
			if trueCalls < 3 {
				return "", errors.New("Operation not supported by device")
			}
			return "", nil
		}
		// `run --root -- systemctl is-system-running`
		if last(args) == "is-system-running" {
			return "degraded\n", nil
		}
		return "", nil
	}}
	c := newCLI(f)
	if err := c.Boot(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if trueCalls < 3 {
		t.Errorf("Boot should retry `true` until execable, got %d calls", trueCalls)
	}
}

func TestBootTimesOut(t *testing.T) {
	f := &fake{handler: func(int, []string) (string, error) {
		return "", errors.New("Operation not supported by device")
	}}
	c := newCLI(f)
	err := c.Boot(context.Background(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never became execable") {
		t.Fatalf("want execable timeout, got %v", err)
	}
}

func TestRecreateDeletesCreatesBoots(t *testing.T) {
	f := &fake{handler: func(n int, args []string) (string, error) {
		if args[1] == "inspect" {
			if n == 0 {
				return "", nil // exists before delete
			}
			return "", errors.New("not found") // gone after delete
		}
		if last(args) == "is-system-running" {
			return "running\n", nil
		}
		return "", nil
	}}
	c := newCLI(f)
	if err := c.Recreate(context.Background(), CreateOpts{CPUs: 2, Memory: "4G", Image: "img"}, 5*time.Second); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	if !f.called("machine delete vpg-a") || !f.called("machine create") {
		t.Error("Recreate should delete then create")
	}
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}
