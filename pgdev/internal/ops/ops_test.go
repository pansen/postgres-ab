package ops_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/ops"
	"pansen.me/pgdev/internal/store"
	"pansen.me/pgdev/internal/task"
)

// These are the acceptance tests for Slice 1: for a snapshot or restore
// interrupted at ANY step boundary (and both before/after the progress record is
// persisted), recovery must converge the filesystem to the before-state or the
// after-state — never a torn in-between — and leave PostgreSQL running and the
// journal empty. This is the guarantee the shell version could not express or
// test.

func testConfig() config.Config {
	return config.Config{BackendPrefix: "pg-dev", DataRoot: "/var/lib/pg-dev-local"}
}

func TestSnapshotConverges(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, before := range []bool{false, true} {
			for at := 1; at <= 6; at++ {
				name := fmt.Sprintf("replace=%v/before=%v/crashAt=%d", replace, before, at)
				t.Run(name, func(t *testing.T) {
					st := &store.Store{Root: t.TempDir(), Clone: copyTree}
					fake := &fakeBackend{running: true, pg: true}
					o := ops.New(st, fake, testConfig())

					must(t, st.EnsureLayout("a"))
					writeData(t, st.Current("a"), "live")
					if replace {
						writeData(t, st.Snapshot("a", "snap1"), "stale")
					}

					tk, err := o.Snapshot(context.Background(), "a", "snap1", replace)
					if err != nil {
						t.Fatalf("build snapshot: %v", err)
					}
					runToCrash(t, st, &tk, at, before)
					recoverAll(t, st, o)

					// current is never touched by snapshot.
					if got := readData(t, st.Current("a")); got != "live" {
						t.Fatalf("current corrupted: %q", got)
					}
					snaps := snapshotSet(t, st)
					switch {
					case len(snaps) == 0 && !replace:
						// rolled back to before-state (no snapshot yet)
					case len(snaps) == 1 && snaps["snap1"] == "live":
						// committed (or rolled forward)
					case replace && len(snaps) == 1 && snaps["snap1"] == "stale":
						// rolled back to before-state (previous snapshot intact)
					default:
						t.Fatalf("torn state: %+v", snaps)
					}
					assertClean(t, st, fake, true)
				})
			}
		}
	}
}

func TestRestoreConverges(t *testing.T) {
	for _, before := range []bool{false, true} {
		for at := 1; at <= 8; at++ {
			name := fmt.Sprintf("before=%v/crashAt=%d", before, at)
			t.Run(name, func(t *testing.T) {
				st := &store.Store{Root: t.TempDir(), Clone: copyTree}
				fake := &fakeBackend{running: true, pg: true}
				o := ops.New(st, fake, testConfig())

				must(t, st.EnsureLayout("a"))
				writeData(t, st.Current("a"), "current")
				makeSnapshot(t, st, "base", "vBASE", 0)
				makeSnapshot(t, st, "mid", "vMID", 1)
				makeSnapshot(t, st, "tip", "vTIP", 2)

				// Restore the OLDEST — this must discard mid+tip on commit.
				tk, err := o.Restore(context.Background(), "a", "base")
				if err != nil {
					t.Fatalf("build restore: %v", err)
				}
				runToCrash(t, st, &tk, at, before)
				recoverAll(t, st, o)

				cur := readData(t, st.Current("a"))
				snaps := snapshotSet(t, st)
				keys := sortedKeys(snaps)
				switch {
				case cur == "current" && keys == "base,mid,tip":
					// rolled back to before-state
				case cur == "vBASE" && keys == "base":
					// committed: current is the restored data, timeline truncated
				default:
					t.Fatalf("torn state: current=%q snapshots=%v", cur, snaps)
				}
				if snaps["base"] != "vBASE" {
					t.Fatalf("base snapshot mutated: %q", snaps["base"])
				}
				assertClean(t, st, fake, false)
				if !fake.running {
					t.Fatal("container should be running after restore recovery")
				}
			})
		}
	}
}

func TestSnapshotAutoStartsPG(t *testing.T) {
	st := &store.Store{Root: t.TempDir(), Clone: copyTree}
	fake := &fakeBackend{running: true, pg: false} // container up, PG down
	o := ops.New(st, fake, testConfig())
	must(t, st.EnsureLayout("a"))
	writeData(t, st.Current("a"), "live")

	tk, err := o.Snapshot(context.Background(), "a", "snap1", false)
	if err != nil {
		t.Fatalf("precheck should auto-start PG, not fail: %v", err)
	}
	if !fake.pg {
		t.Fatal("PG should have been started by the precheck")
	}
	j, err := task.NewFileJournal(st.JournalDir())
	must(t, err)
	if err := task.Run(context.Background(), j, tk); err != nil {
		t.Fatalf("run: %v", err)
	}
	if snapshotSet(t, st)["snap1"] != "live" {
		t.Fatal("snapshot not created")
	}
}

func TestSnapshotRequiresContainer(t *testing.T) {
	st := &store.Store{Root: t.TempDir(), Clone: copyTree}
	fake := &fakeBackend{running: false, pg: false} // container down
	o := ops.New(st, fake, testConfig())
	must(t, st.EnsureLayout("a"))
	writeData(t, st.Current("a"), "live")
	if _, err := o.Snapshot(context.Background(), "a", "snap1", false); err == nil {
		t.Fatal("expected an error when the container is not running")
	}
}

// ----- crash harness -------------------------------------------------------

var errCrash = errors.New("simulated crash")

// crashJournal wraps a real FileJournal and panics on the at-th Advance, either
// just before persisting it (before=true → models a kill after a step's Do but
// before its progress record) or just after (before=false → a clean between-step
// kill).
type crashJournal struct {
	inner  *task.FileJournal
	n, at  int
	before bool
}

func (c *crashJournal) Begin(r task.Record) error { return c.inner.Begin(r) }
func (c *crashJournal) Commit(id string) error    { return c.inner.Commit(id) }
func (c *crashJournal) Pending() ([]task.Record, error) {
	return c.inner.Pending()
}
func (c *crashJournal) Advance(id string, completed int) error {
	c.n++
	if c.before && c.n == c.at {
		panic(errCrash)
	}
	err := c.inner.Advance(id, completed)
	if !c.before && c.n == c.at {
		panic(errCrash)
	}
	return err
}

func runToCrash(t *testing.T, st *store.Store, tk *task.Task, at int, before bool) {
	t.Helper()
	inner, err := task.NewFileJournal(st.JournalDir())
	if err != nil {
		t.Fatal(err)
	}
	cj := &crashJournal{inner: inner, at: at, before: before}
	defer func() {
		if r := recover(); r != nil && r != errCrash {
			panic(r)
		}
	}()
	_ = task.Run(context.Background(), cj, *tk) // may panic (simulated crash) or complete
}

func recoverAll(t *testing.T, st *store.Store, o *ops.Ops) {
	t.Helper()
	j, err := task.NewFileJournal(st.JournalDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Recover(context.Background(), j, o.Registry()); err != nil {
		t.Fatalf("recover: %v", err)
	}
}

// ----- fake backend --------------------------------------------------------

type fakeBackend struct {
	running bool
	pg      bool
}

func (f *fakeBackend) ContainerRunning(context.Context, string) (bool, error) {
	return f.running, nil
}
func (f *fakeBackend) PGActive(context.Context, string) (bool, error) { return f.running && f.pg, nil }
func (f *fakeBackend) StopPG(context.Context, string) error           { f.pg = false; return nil }
func (f *fakeBackend) EnsurePGRunning(context.Context, string) error {
	if f.running {
		f.pg = true
	}
	return nil
}
func (f *fakeBackend) StopContainer(context.Context, string) error {
	f.running, f.pg = false, false
	return nil
}
func (f *fakeBackend) StopContainerForce(context.Context, string) error {
	f.running, f.pg = false, false
	return nil
}
func (f *fakeBackend) StartContainerAndWait(context.Context, string) error {
	f.running, f.pg = true, true
	return nil
}
func (f *fakeBackend) RepairIP(context.Context, string) error { return nil }

// ----- fs helpers ----------------------------------------------------------

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeData(t *testing.T, dir, content string) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "data"), []byte(content), 0o600))
}

func readData(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return string(b)
}

func makeSnapshot(t *testing.T, st *store.Store, name, content string, order int) {
	t.Helper()
	p := st.Snapshot("a", name)
	writeData(t, p, content)
	ts := time.Unix(1_700_000_000+int64(order)*60, 0)
	must(t, os.Chtimes(p, ts, ts))
}

func snapshotSet(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	snaps, err := st.List("a")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range snaps {
		out[s.Name] = readData(t, st.Snapshot("a", s.Name))
	}
	return out
}

func sortedKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// assertClean verifies no transaction artifacts leaked and the journal is empty.
func assertClean(t *testing.T, st *store.Store, fake *fakeBackend, wantPG bool) {
	t.Helper()
	// No stray dirs beside current/ and snapshots/ (e.g. current.restore-new/old).
	entries, err := os.ReadDir(st.SlotDir("a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "current" && e.Name() != "snapshots" {
			t.Fatalf("leftover slot artifact: %s", e.Name())
		}
	}
	// No hidden staging entries in snapshots/ (.stage.*, .replaced.*, .restore-prune).
	snapEntries, err := os.ReadDir(st.SnapshotsDir("a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snapEntries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover snapshot staging artifact: %s", e.Name())
		}
	}
	// Journal fully resolved.
	j, err := task.NewFileJournal(st.JournalDir())
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := j.Pending(); len(p) != 0 {
		t.Fatalf("journal not cleared: %+v", p)
	}
	if wantPG && !fake.pg {
		t.Fatal("PostgreSQL should be running after recovery")
	}
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
