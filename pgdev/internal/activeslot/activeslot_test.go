package activeslot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsToA(t *testing.T) {
	p := Pointer{Path: filepath.Join(t.TempDir(), "active-slot")}
	if got := p.Get(); got != "a" {
		t.Fatalf("Get() on missing file = %q, want a", got)
	}
	if got := p.Staging(); got != "b" {
		t.Fatalf("Staging() = %q, want b", got)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	p := Pointer{Path: filepath.Join(t.TempDir(), "active-slot")}
	if err := p.Set("b"); err != nil {
		t.Fatal(err)
	}
	if got := p.Get(); got != "b" {
		t.Fatalf("Get() = %q, want b", got)
	}
	if got := p.Staging(); got != "a" {
		t.Fatalf("Staging() = %q, want a", got)
	}
}

func TestSetRejectsInvalid(t *testing.T) {
	p := Pointer{Path: filepath.Join(t.TempDir(), "active-slot")}
	if err := p.Set("c"); err == nil {
		t.Fatal("expected rejection of slot c")
	}
}

// A garbage file reads back as the default rather than propagating junk.
func TestGetToleratesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active-slot")
	if err := os.WriteFile(path, []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (Pointer{Path: path}).Get(); got != "a" {
		t.Fatalf("Get() on garbage = %q, want a", got)
	}
}
