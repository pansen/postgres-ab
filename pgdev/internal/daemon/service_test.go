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
