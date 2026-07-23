package forward

import (
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
)

// TestClassifyDial locks the error buckets and, crucially, that the EHOSTUNREACH
// case surfaces the macOS Local Network Privacy remediation — the whole reason
// this classification exists (that failure otherwise reads as an inscrutable
// "no route to host" while a terminal dial to the same IP works).
func TestClassifyDial(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantClass  string
		hintSubstr string // "" = expect no hint
	}{
		{"ehostunreach", wrapDial(syscall.EHOSTUNREACH), "EHOSTUNREACH", "Local Network"},
		{"econnrefused", wrapDial(syscall.ECONNREFUSED), "ECONNREFUSED", "nothing is listening"},
		{"enetunreach", wrapDial(syscall.ENETUNREACH), "ENETUNREACH", "no route to the VM subnet"},
		{"timeout", timeoutErr{}, "timeout", "dial timeout"},
		{"other", fmt.Errorf("boom"), "other", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, hint := classifyDial(tc.err)
			if class != tc.wantClass {
				t.Fatalf("class = %q, want %q", class, tc.wantClass)
			}
			if tc.hintSubstr == "" {
				if hint != "" {
					t.Fatalf("expected no hint, got %q", hint)
				}
				return
			}
			if !strings.Contains(hint, tc.hintSubstr) {
				t.Fatalf("hint %q does not mention %q", hint, tc.hintSubstr)
			}
		})
	}
}

// wrapDial mirrors how the kernel error reaches us — inside a *net.OpError, the
// way net.DialTimeout returns it — to prove errors.Is unwraps through it.
func wrapDial(sys syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.OpError{Op: "connect", Err: sys}}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0B"}, {512, "512B"}, {1024, "1.0KB"}, {1536, "1.5KB"}, {1048576, "1.0MB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
