package forward

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// State is the forwarder's live self-report, written by a running `serve` on
// every poll tick. It lets `promote`/`forward status` observe the EFFECTIVE
// mapping (and the forwarder's liveness via UpdatedUnix) without a launchd
// round-trip — restoring the safety `promote` used to get from re-point failing
// loudly, now that promote is just a pointer write.
type State struct {
	PID         int    `json:"pid"`
	ActiveSlot  string `json:"active_slot"`
	Active      string `json:"active_target"`
	Staging     string `json:"staging_target"`
	UpdatedUnix int64  `json:"updated_unix"`
}

// Fresh reports whether the state was written recently enough to trust the
// forwarder is alive and serving this mapping.
func (s State) Fresh(within time.Duration) bool {
	return time.Since(time.Unix(s.UpdatedUnix, 0)) <= within
}

func (s *Server) writeState(slot, active, staging string) {
	if s.opts.StatePath == "" {
		return
	}
	writeStateFile(s.opts.StatePath, State{
		PID:         os.Getpid(),
		ActiveSlot:  slot,
		Active:      active,
		Staging:     staging,
		UpdatedUnix: time.Now().Unix(),
	})
}

func writeStateFile(path string, st State) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, "forward-state.*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(name, path)
}

// ReadState loads the forwarder's last self-report. ok is false when the file is
// absent or unparsable (no forwarder has ever run, or it is not writing).
func ReadState(path string) (State, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, false
	}
	return st, true
}

// ReapListeners kills any *socat* process still LISTENing on the given TCP
// ports. This is a ONE-TIME migration step for `forward install`: orphaned socat
// children from the retired shell forwarder may still hold :5442/:5443, and a
// fresh `serve` would get EADDRINUSE and crash-loop under KeepAlive. The
// steady-state design never orphans anything, so this is not on the serve path.
// It filters on the socat command name (lsof -c) so it can NEVER kill an
// unrelated process that happens to hold those ports (e.g. after the user
// re-points PG_CLIENT_*_PORT). Best-effort and macOS-specific (lsof); a no-op
// where lsof is unavailable or no socat is found.
func ReapListeners(ports ...int) {
	// -a ANDs the selectors: (command=socat) AND (one of the ports) AND LISTEN.
	args := []string{"-nP", "-t", "-a", "-c", "socat", "-sTCP:LISTEN"}
	for _, p := range ports {
		args = append(args, "-iTCP:"+strconv.Itoa(p))
	}
	out, err := exec.Command("lsof", args...).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil && pid != os.Getpid() {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
			}
		}
	}
}
