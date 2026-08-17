package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskBytesCountsTree(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "base", "5")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	const size = 64 * 1024
	for _, name := range []string{"16384", "16385"} {
		if err := os.WriteFile(filepath.Join(sub, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiskBytes(dir)
	if err != nil {
		t.Fatalf("DiskBytes: %v", err)
	}
	// Allocation is filesystem-dependent (tails, directory blocks), so assert
	// the payload is accounted for rather than an exact figure.
	if got < 2*size {
		t.Errorf("DiskBytes = %d, want at least %d", got, 2*size)
	}
}

func TestDiskBytesMissingRoot(t *testing.T) {
	if _, err := DiskBytes(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
