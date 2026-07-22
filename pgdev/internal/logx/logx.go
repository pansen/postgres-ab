// Package logx is a minimal progress logger. Messages go to stderr with the
// same "==> " prefix the shell scripts use, so they stream back through the
// machine transport and read consistently with the rest of make's output.
package logx

import (
	"fmt"
	"os"
)

// Func prints a progress line. A nil Func means "discard" (see Or).
type Func func(format string, args ...any)

// Stderr writes a "==> " progress line to standard error.
func Stderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...)
}

// Discard drops the message.
func Discard(string, ...any) {}

// Or returns f, or Discard when f is nil — so callers can log unconditionally.
func Or(f Func) Func {
	if f == nil {
		return Discard
	}
	return f
}
