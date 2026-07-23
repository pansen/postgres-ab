// Package forward is the host-side (macOS) client forwarder that replaces the
// shell socat relay (scripts/host-endpoint, retired in spec 0003). It owns the
// two stable loopback listeners — 127.0.0.1:5442 (active) and :5443 (staging) —
// for their whole lifetime and re-points by swapping the dial target IN PLACE,
// never rebinding. That kills the socat/launchd process-lifecycle bugs: nothing
// external to orphan, no "Address already in use" on re-point, no SIGKILL
// trap-bypass. `promote` then collapses to a pointer write — the running
// forwarder picks the change up within its poll interval.
//
// The two Apple machines expose one PostgreSQL backend each on their own
// eth0:5432; their IPs drift and cannot be pinned, so the forwarder tracks the
// host-side caches var/machine-ip-{a,b} and the active-machine pointer
// (var/active-machine, "a"/"b") and maps each client port onto whichever
// machine currently holds that role.
package forward

import (
	"net"
	"strconv"
)

// Targets computes the active and staging dial targets ("ip:port") from the
// active-slot pointer and the two cached machine IPs. Staging is always the
// other slot. An empty IP yields an empty target, marking that role unroutable
// (its machine is down or has never reported an address) — a missing IP for one
// role never affects the other. Any activeSlot other than "b" is treated as "a",
// matching activeslot.Pointer's default.
func Targets(activeSlot, ipA, ipB string, backendPort int) (active, staging string) {
	if activeSlot == "b" {
		return target(ipB, backendPort), target(ipA, backendPort)
	}
	return target(ipA, backendPort), target(ipB, backendPort)
}

func target(ip string, port int) string {
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, strconv.Itoa(port))
}
