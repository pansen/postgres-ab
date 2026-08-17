package main

import (
	"strings"
	"testing"

	"pansen.me/pgdev/internal/agentapi"
)

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 bytes"},
		{999, "999 bytes"},
		{1024, "1.00 kB"},
		{1536, "1.50 kB"},
		{5 * 1024 * 1024, "5.00 MB"},
		{11 * 1024 * 1024 * 1024, "11.00 GB"},
		{3 * 1024 * 1024 * 1024 * 1024, "3.00 TB"},
		{4096 * 1024 * 1024 * 1024 * 1024, "4096.00 TB"}, // no unit beyond TB
	}
	for _, c := range cases {
		if got := fmtBytes(c.n); got != c.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestDBSizeLine(t *testing.T) {
	line := dbSizeLine("vpg", agentapi.DBSize{SQLBytes: 11 * 1024 * 1024 * 1024, DiskBytes: 13 * 1024 * 1024 * 1024})
	want := "database vpg: 11.00 GB (SQL) / 13.00 GB on disk"
	if line != want {
		t.Errorf("dbSizeLine = %q, want %q", line, want)
	}
}

// A stopped backend still has a measurable data directory, so the on-disk figure
// must survive an unavailable SQL figure.
func TestDBSizeLineSQLUnavailable(t *testing.T) {
	line := dbSizeLine("vpg", agentapi.DBSize{DiskBytes: 8 * 1024 * 1024 * 1024, SQLError: "backend not running"})
	want := "database vpg: - (SQL: backend not running) / 8.00 GB on disk"
	if line != want {
		t.Errorf("dbSizeLine = %q, want %q", line, want)
	}
}

// A daemon predating the size fields sends the zero value; that must not read as
// an empty database.
func TestDBSizeLineOldDaemon(t *testing.T) {
	line := dbSizeLine("vpg", agentapi.DBSize{})
	if strings.Contains(line, "0 bytes") {
		t.Errorf("zero-value DBSize rendered as a real size: %q", line)
	}
	if !strings.Contains(line, "make deploy") {
		t.Errorf("dbSizeLine should point at the fix: %q", line)
	}
}

func TestShortErr(t *testing.T) {
	if got := shortErr("exec in pg-dev-a: exit status 1\npsql: FATAL: ...\n"); got != "exec in pg-dev-a: exit status 1" {
		t.Errorf("shortErr kept more than the first line: %q", got)
	}
	got := shortErr(strings.Repeat("x", 200))
	if len(got) != 60 || !strings.HasSuffix(got, "...") {
		t.Errorf("shortErr(long) = %q (len %d), want 60 chars ending in ...", got, len(got))
	}
}
