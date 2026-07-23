package forward

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
