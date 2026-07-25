package logx

import (
	"os"
	"strings"
)

// ----- when to colorize -------------------------------------------------------
//
// The colored rendering itself is lmittmann/tint's job (a drop-in slog.Handler);
// tint has no terminal detection of its own, so deciding whether to hand it
// NoColor is ours. Kept here — dependency-free, so the in-machine daemon that
// imports this package for its progress lines picks up nothing extra.

// UseColor decides whether ANSI escapes should be emitted for f, honoring (in
// order): mode "always"/"never" (PG_LOG_COLOR), the NO_COLOR convention
// (https://no-color.org), a dumb or unset TERM, and finally whether f is a
// terminal at all — so a redirected or piped run is never polluted with escapes.
func UseColor(mode string, f *os.File) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always", "force", "1", "yes", "true":
		return true
	case "never", "off", "0", "no", "false":
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if term := os.Getenv("TERM"); term == "" || term == "dumb" {
		return false
	}
	return IsTerminal(f)
}

// IsTerminal reports whether f is a character device (a tty). Deliberately
// dependency-free: the character-device bit is what golang.org/x/term's isatty
// resolves to on Darwin/Linux for this purpose.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
