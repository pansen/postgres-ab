//go:build integration

package store

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestXFSReflinkClone exercises the REAL reflink path against a genuine XFS
// filesystem (the shell version's snapshot substrate). It builds a loop-backed
// `mkfs.xfs -m reflink=1` filesystem, then verifies StageClone's
// `cp --reflink=always` succeeds and reproduces the data — a path impossible to
// cover on the dev Mac.
//
// Run on a Linux CI runner with privileges:
//
//	sudo go test -tags=integration -run XFS ./internal/store/...
func TestXFSReflinkClone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root (losetup/mkfs.xfs/mount)")
	}
	for _, bin := range []string{"losetup", "mkfs.xfs", "mount", "umount", "truncate"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("missing %s", bin)
		}
	}

	work := t.TempDir()
	img := filepath.Join(work, "store.xfs")
	mnt := filepath.Join(work, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}

	runCmd(t, "truncate", "-s", "512M", img)
	loop := outCmd(t, "losetup", "--find", "--show", img)
	defer runCmd(t, "losetup", "--detach", loop)
	runCmd(t, "mkfs.xfs", "-q", "-m", "reflink=1", loop)
	runCmd(t, "mount", loop, mnt)
	defer runCmd(t, "umount", mnt)

	st := NewOSStore(mnt)
	if err := st.RequireMounted(); err != nil {
		t.Fatalf("RequireMounted: %v", err)
	}
	if err := st.EnsureLayout("a"); err != nil {
		t.Fatal(err)
	}

	// Write a few MiB so a full copy would be observably distinct from a reflink.
	payload := bytes.Repeat([]byte("pgdev-reflink-probe\n"), 200_000)
	if err := os.WriteFile(filepath.Join(st.Current("a"), "data"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	// StageClone uses the production reflinkClone (cp --reflink=always), which
	// FAILS outright if the filesystem cannot do CoW — so success is the signal.
	dst := st.Snapshot("a", "snap1")
	if err := st.StageClone(context.Background(), st.Current("a"), dst); err != nil {
		t.Fatalf("reflink StageClone: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("cloned data differs from source")
	}
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func outCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(bytes.TrimSpace(out))
}
