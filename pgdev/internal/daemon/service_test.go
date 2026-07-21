package daemon

import (
	"context"
	"testing"

	"pansen.me/pgdev/internal/config"
)

// With both backend IPs pinned in .env, resolution is pure config — no incusbr0
// lookup, so the daemon needs no live Incus to build a blueprint.
func TestResolveBackendIPsUsesOverrides(t *testing.T) {
	s := &Service{Cfg: config.Config{BackendAIP: "10.9.9.11", BackendBIP: "10.9.9.12"}}
	a, b, err := s.resolveBackendIPs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a != "10.9.9.11" || b != "10.9.9.12" {
		t.Fatalf("resolved %s/%s, want the overrides", a, b)
	}
}

func TestOtherSlot(t *testing.T) {
	if other("a") != "b" || other("b") != "a" {
		t.Fatal("other() should flip a↔b")
	}
}
