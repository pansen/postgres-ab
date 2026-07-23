package daemon

import (
	"testing"

	"pansen.me/pgdev/internal/config"
)

// Each daemon serves exactly one backend, chosen by PG_SLOT.
func TestSlotAndContainer(t *testing.T) {
	sb := &Service{Cfg: config.Config{BackendPrefix: "pg-dev", Slot: "b"}}
	if sb.slot() != "b" || sb.container() != "pg-dev-b" {
		t.Fatalf("slot=%q container=%q", sb.slot(), sb.container())
	}
	// Unset slot defaults to "a" so a legacy/single-machine deploy still resolves.
	sa := &Service{Cfg: config.Config{BackendPrefix: "pg-dev", Slot: ""}}
	if sa.slot() != "a" || sa.container() != "pg-dev-a" {
		t.Fatalf("default slot=%q container=%q", sa.slot(), sa.container())
	}
}

// New fails fast on a misconfigured slot instead of silently defaulting to "a".
func TestNewRejectsInvalidSlot(t *testing.T) {
	if _, err := New(config.Config{Slot: "x"}, "v", nil); err == nil {
		t.Fatal("New should reject an invalid PG_SLOT")
	}
	// Unset and a/b are accepted (unset → legacy default).
	for _, s := range []string{"", "a", "b"} {
		if _, err := New(config.Config{Slot: s, DataRoot: t.TempDir()}, "v", nil); err != nil {
			t.Fatalf("New(slot=%q): %v", s, err)
		}
	}
}
