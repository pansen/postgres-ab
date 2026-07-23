package blueprint

import (
	"testing"

	"pansen.me/pgdev/internal/config"
)

func cfg() config.Config {
	return config.Config{BackendPrefix: "pg-dev", BackendPort: 5432}
}

// One backend per machine: the forward exposes it on the machine's eth0 and
// connects to PostgreSQL on the container's loopback (no IP pinning to drift).
func TestComputeForwardAndBackend(t *testing.T) {
	bp := Compute(cfg(), "a")
	if bp.Backend.Name != "pg-dev-a" || bp.Backend.Slot != "a" {
		t.Fatalf("backend = %+v", bp.Backend)
	}
	if bp.Forward.Device != ForwardDevice {
		t.Fatalf("forward device = %q", bp.Forward.Device)
	}
	if bp.Forward.Listen != "tcp:0.0.0.0:5432" {
		t.Fatalf("listen = %q", bp.Forward.Listen)
	}
	if bp.Forward.Connect != "tcp:127.0.0.1:5432" {
		t.Fatalf("connect = %q, want container loopback", bp.Forward.Connect)
	}
}

func TestComputeSlotB(t *testing.T) {
	bp := Compute(cfg(), "b")
	if bp.Backend.Name != "pg-dev-b" || bp.Backend.Slot != "b" {
		t.Fatalf("backend = %+v", bp.Backend)
	}
}
