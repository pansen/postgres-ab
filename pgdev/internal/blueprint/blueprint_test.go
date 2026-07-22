package blueprint

import (
	"testing"

	"pansen.me/pgdev/internal/config"
)

func cfg() config.Config {
	return config.Config{
		BackendPrefix: "pg-dev",
		ProxyName:     "pg-proxy",
		ActivePort:    5432,
		StagingPort:   5433,
	}
}

// The whole promote-collapse rests on this: the proxy connect targets are a pure
// function of the active slot. Active=a → :5432 dials a's IP; flip to b and the
// same listener now dials b, with staging following the other way.
func TestComputeConnectTargetsFollowActive(t *testing.T) {
	aIP, bIP := "10.0.0.11", "10.0.0.12"

	bpA := Compute(cfg(), "a", aIP, bIP)
	if got := device(bpA, MainDevice).Connect; got != "tcp:10.0.0.11:5432" {
		t.Fatalf("active=a main connect = %q, want a's IP", got)
	}
	if got := device(bpA, StagingDevice).Connect; got != "tcp:10.0.0.12:5432" {
		t.Fatalf("active=a staging connect = %q, want b's IP", got)
	}

	bpB := Compute(cfg(), "b", aIP, bIP)
	if got := device(bpB, MainDevice).Connect; got != "tcp:10.0.0.12:5432" {
		t.Fatalf("active=b main connect = %q, want b's IP", got)
	}
	if got := device(bpB, StagingDevice).Connect; got != "tcp:10.0.0.11:5432" {
		t.Fatalf("active=b staging connect = %q, want a's IP", got)
	}
}

func TestComputeListenPortsAndBackends(t *testing.T) {
	bp := Compute(cfg(), "a", "10.0.0.11", "10.0.0.12")
	if got := device(bp, MainDevice).Listen; got != "tcp:0.0.0.0:5432" {
		t.Fatalf("main listen = %q", got)
	}
	if got := device(bp, StagingDevice).Listen; got != "tcp:0.0.0.0:5433" {
		t.Fatalf("staging listen = %q", got)
	}
	if bp.Backends[0].Name != "pg-dev-a" || bp.Backends[1].Name != "pg-dev-b" {
		t.Fatalf("backend names = %q/%q", bp.Backends[0].Name, bp.Backends[1].Name)
	}
	if bp.Proxy.Name != "pg-proxy" {
		t.Fatalf("proxy name = %q", bp.Proxy.Name)
	}
}

func device(bp Blueprint, name string) ProxyDevice {
	for _, d := range bp.Proxy.Devices {
		if d.Name == name {
			return d
		}
	}
	return ProxyDevice{}
}
