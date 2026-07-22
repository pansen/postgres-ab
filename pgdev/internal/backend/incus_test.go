package backend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// wrappedExitError reproduces how Incus.run wraps a failing `incus` invocation:
// a real *exec.ExitError behind a fmt.Errorf("%w"). This is the exact shape that
// once fooled a bare type assertion into misclassifying a stopped unit.
func wrappedExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("false").Run() // deterministic non-zero exit
	if err == nil {
		t.Skip("`false` did not exit non-zero")
	}
	return fmt.Errorf("incus exec: %w", err)
}

func TestPGActiveClassifiesExit(t *testing.T) {
	ctx := context.Background()

	// Stopped unit: systemctl is-active exits non-zero → not active, no error.
	i := &Incus{runFn: func(context.Context, ...string) error { return wrappedExitError(t) }}
	if active, err := i.PGActive(ctx, "pg-dev-a"); active || err != nil {
		t.Fatalf("stopped unit: got (active=%v, err=%v), want (false, nil)", active, err)
	}

	// Active unit: exit 0 → active.
	i = &Incus{runFn: func(context.Context, ...string) error { return nil }}
	if active, err := i.PGActive(ctx, "pg-dev-a"); !active || err != nil {
		t.Fatalf("active unit: got (active=%v, err=%v), want (true, nil)", active, err)
	}

	// Transport failure (not an ExitError): surfaced as an error.
	sentinel := errors.New("socket unreachable")
	i = &Incus{runFn: func(context.Context, ...string) error { return sentinel }}
	if _, err := i.PGActive(ctx, "pg-dev-a"); !errors.Is(err, sentinel) {
		t.Fatalf("transport failure: got err=%v, want %v", err, sentinel)
	}
}
