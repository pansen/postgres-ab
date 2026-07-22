package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMoveIdempotent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	mustMkdir(t, a)

	if err := Move(a, b); err != nil { // normal move
		t.Fatalf("move a->b: %v", err)
	}
	if Exists(a) || !Exists(b) {
		t.Fatal("a should be gone, b present")
	}
	if err := Move(a, b); err != nil { // already moved (src gone, dst present) → no-op
		t.Fatalf("idempotent move: %v", err)
	}
	// both exist → error
	mustMkdir(t, a)
	if err := Move(a, b); err == nil {
		t.Fatal("expected error when both exist")
	}
	// neither exists → error
	os.RemoveAll(a)
	os.RemoveAll(b)
	if err := Move(a, b); err == nil {
		t.Fatal("expected error when neither exists")
	}
}

func TestMatchTopMode(t *testing.T) {
	dir := t.TempDir()
	ref, dst := filepath.Join(dir, "ref"), filepath.Join(dir, "dst")
	mustMkdir(t, ref)
	mustMkdir(t, dst)
	if err := os.Chmod(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MatchTopMode(ref, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("dst mode = %o, want 0755", fi.Mode().Perm())
	}
}

func TestValidateName(t *testing.T) {
	ok := []string{"initial", "2026-07-20T10-00-00_dump", "a.b_c-d", "X"}
	bad := []string{"", ".hidden", "-leading", "has space", "a/b", "a$b"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestListAndAfterOrdering(t *testing.T) {
	st := &Store{Root: t.TempDir(), Clone: copyTree}
	if err := st.EnsureLayout("a"); err != nil {
		t.Fatal(err)
	}
	// Create three snapshots plus a hidden staging dir; force known mtimes.
	base := time.Unix(1_600_000_000, 0)
	for i, name := range []string{"first", "second", "third"} {
		p := st.Snapshot("a", name)
		mustMkdir(t, p)
		if err := os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(t, st.Snapshot("a", ".stage.ignored")) // must be skipped

	snaps, err := st.List("a")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(snaps); join(got) != "first,second,third" {
		t.Fatalf("List order = %v", got)
	}

	after, err := st.After("a", "first")
	if err != nil {
		t.Fatal(err)
	}
	if join(after) != "second,third" {
		t.Fatalf("After(first) = %v", after)
	}
	if last, _ := st.Last("a"); last != "third" {
		t.Fatalf("Last = %q", last)
	}
	if after, _ := st.After("a", "third"); len(after) != 0 {
		t.Fatalf("After(third) = %v, want empty", after)
	}
}

// TestTouchNowFixesCloneMtimeOrdering reproduces the snapshot-ordering bug: a
// `cp -a` reflink clone preserves the SOURCE data dir's mtime, so every snapshot
// shares one stale timestamp and List falls back to lexical order — pushing an
// early "initial" snapshot to the end. Stamping each with TouchNow at creation
// (as the snapshot task does) restores true creation order.
func TestTouchNowFixesCloneMtimeOrdering(t *testing.T) {
	st := &Store{Root: t.TempDir(), Clone: copyTree}
	if err := st.EnsureLayout("a"); err != nil {
		t.Fatal(err)
	}
	stale := time.Unix(1_600_000_000, 0)
	// Creation order is initial → dump → later; names sort lexically the other way.
	for _, name := range []string{"initial", "2026-07-21_dump", "2026-07-22_later"} {
		p := st.Snapshot("a", name)
		mustMkdir(t, p)
		if err := os.Chtimes(p, stale, stale); err != nil { // the cp -a symptom
			t.Fatal(err)
		}
		if err := TouchNow(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // distinct mtimes across snapshots
	}

	if got := join(names(mustList(t, st))); got != "initial,2026-07-21_dump,2026-07-22_later" {
		t.Fatalf("List order = %q, want creation order (initial first)", got)
	}
	if last, _ := st.Last("a"); last != "2026-07-22_later" {
		t.Fatalf("Last = %q, want 2026-07-22_later", last)
	}
	if after, _ := st.After("a", "initial"); join(after) != "2026-07-21_dump,2026-07-22_later" {
		t.Fatalf("After(initial) = %v, want the two newer snapshots", after)
	}
}

// ----- test helpers --------------------------------------------------------

func mustList(t *testing.T, st *Store) []Snapshot {
	t.Helper()
	snaps, err := st.List("a")
	if err != nil {
		t.Fatal(err)
	}
	return snaps
}


func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func names(s []Snapshot) []string {
	out := make([]string, len(s))
	for i, sn := range s {
		out[i] = sn.Name
	}
	return out
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// copyTree is a portable Cloner for tests (no reflink dependency).
func copyTree(_ context.Context, src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}
