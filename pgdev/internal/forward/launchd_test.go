package forward

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// healthyPrint mimics `launchctl print gui/<uid>/<label>` for a loaded, running
// agent whose ProgramArguments are the current pgdev binary.
const healthyPrint = `gui/503/me.pansen.vpg-forward = {
	active count = 1
	path = /Users/a/Library/LaunchAgents/me.pansen.vpg-forward.plist
	state = running
	program = /Users/a/p/pansen/postgres-ab/pgdev/bin/pgdev
	arguments = {
		/Users/a/p/pansen/postgres-ab/pgdev/bin/pgdev
		forward
		serve
	}
	pid = 42
	last exit code = 0
}`

// stalePrint mimics the real trap: a plist from the repo's previous location,
// loaded but crash-looping on a deleted binary (state = spawn scheduled, no pid).
const stalePrint = `gui/503/me.pansen.vpg-forward = {
	active count = 0
	state = spawn scheduled
	program = /Users/a/p/pansen/colima-incus/scripts/host-endpoint
	arguments = {
		/Users/a/p/pansen/colima-incus/scripts/host-endpoint
		_serve
	}
	last exit code = 78: EX_CONFIG
}`

func TestParsePrintHealthy(t *testing.T) {
	got := parsePrint(healthyPrint)
	wantArgs := []string{
		"/Users/a/p/pansen/postgres-ab/pgdev/bin/pgdev", "forward", "serve",
	}
	if !reflect.DeepEqual(got.args, wantArgs) {
		t.Fatalf("args = %v, want %v", got.args, wantArgs)
	}
	if got.program != "/Users/a/p/pansen/postgres-ab/pgdev/bin/pgdev" {
		t.Fatalf("program = %q", got.program)
	}
	if !got.running {
		t.Fatalf("running = false, want true (pid present)")
	}
	if _, stale := got.stale(wantArgs); stale {
		t.Fatalf("healthy agent reported stale against its own argv")
	}
}

func TestParsePrintStale(t *testing.T) {
	got := parsePrint(stalePrint)
	if got.running {
		t.Fatalf("running = true, want false (no pid, spawn scheduled)")
	}
	want := []string{"/Users/a/p/pansen/postgres-ab/pgdev/bin/pgdev", "forward", "serve"}
	observed, stale := got.stale(want)
	if !stale {
		t.Fatalf("stale plist not detected as stale (observed %q)", observed)
	}
	if !strings.Contains(observed, "host-endpoint") {
		t.Fatalf("observed program = %q, want it to name the old host-endpoint", observed)
	}
}

// TestStaleNoEvidence guards the conservative fallback: when print yielded
// neither an arguments block nor a program line, stale() must NOT reinstall.
func TestStaleNoEvidence(t *testing.T) {
	var p printInfo
	if _, stale := p.stale([]string{"/x/pgdev", "forward", "serve"}); stale {
		t.Fatalf("empty printInfo reported stale — would trigger a needless reinstall")
	}
}

// TestStaleProgramFallback covers a print that surfaced the scalar program line
// but no arguments block: the path alone must still detect drift.
func TestStaleProgramFallback(t *testing.T) {
	p := printInfo{program: "/old/repo/bin/pgdev"}
	if _, stale := p.stale([]string{"/new/repo/bin/pgdev", "forward", "serve"}); !stale {
		t.Fatalf("scalar-program drift not detected")
	}
	p = printInfo{program: "/new/repo/bin/pgdev"}
	if _, stale := p.stale([]string{"/new/repo/bin/pgdev", "forward", "serve"}); stale {
		t.Fatalf("matching scalar program reported stale")
	}
}

// TestWritePlistEscapesXML ensures interpolated paths with XML metacharacters
// produce a valid (escaped) plist rather than corrupt markup that would make
// bootstrap fail in a baffling way.
func TestWritePlistEscapesXML(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "a & b <repo>")
	ld := &Launchd{
		Label:    "me.pansen.vpg-forward",
		Plist:    filepath.Join(dir, "agent.plist"),
		Program:  []string{filepath.Join(repo, "bin/pgdev"), "forward", "serve"},
		LogPath:  filepath.Join(repo, "var/vpg-forward.log"),
		RepoRoot: repo,
	}
	if err := ld.writePlist(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ld.Plist)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)

	// The raw metacharacters must not appear unescaped inside the rendered plist.
	if strings.Contains(out, "a & b") || strings.Contains(out, "<repo>") {
		t.Fatalf("plist contains unescaped XML metacharacters:\n%s", out)
	}
	// ...and their escaped forms must be present.
	if !strings.Contains(out, "a &amp; b") || !strings.Contains(out, "&lt;repo&gt;") {
		t.Fatalf("plist is missing the escaped values:\n%s", out)
	}
}
