package logx

import (
	"os"
	"testing"
)

func TestUseColorModes(t *testing.T) {
	// An explicit mode wins over everything, including a non-terminal sink.
	if !UseColor("always", nil) {
		t.Error(`mode "always" must force color`)
	}
	if UseColor("never", os.Stdout) {
		t.Error(`mode "never" must disable color`)
	}
	// auto: NO_COLOR and a dumb TERM each veto, and a plain file is not a tty.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if UseColor("auto", os.Stdout) {
		t.Error("NO_COLOR must disable color")
	}
	os.Unsetenv("NO_COLOR")
	t.Setenv("TERM", "dumb")
	if UseColor("auto", os.Stdout) {
		t.Error("TERM=dumb must disable color")
	}
	t.Setenv("TERM", "xterm-256color")
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if UseColor("auto", f) {
		t.Error("a regular file is not a terminal")
	}
	if IsTerminal(nil) {
		t.Error("a nil file is not a terminal")
	}
}
