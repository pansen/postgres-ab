package main

import (
	"reflect"
	"testing"

	"pansen.me/pgdev/internal/agentapi"
)

func TestSnapshotsAfter(t *testing.T) {
	snaps := []agentapi.SnapshotInfo{
		{Name: "initial"},
		{Name: "mid"},
		{Name: "latest"},
	}
	cases := []struct {
		name string
		want []string
	}{
		{"initial", []string{"mid", "latest"}},
		{"mid", []string{"latest"}},
		{"latest", nil},
		{"missing", nil},
	}
	for _, c := range cases {
		if got := snapshotsAfter(snaps, c.name); !reflect.DeepEqual(got, c.want) {
			t.Errorf("snapshotsAfter(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSnapshotsAfterEmpty(t *testing.T) {
	if got := snapshotsAfter(nil, "initial"); got != nil {
		t.Errorf("snapshotsAfter(nil, ...) = %v, want nil", got)
	}
}

func TestSlotsFor(t *testing.T) {
	cases := []struct {
		machine string
		want    []string
	}{
		{"", []string{"a", "b"}},
		{"both", []string{"a", "b"}},
		{"a", []string{"a"}},
		{"b", []string{"b"}},
	}
	for _, c := range cases {
		got, err := slotsFor(c.machine)
		if err != nil {
			t.Fatalf("slotsFor(%q) unexpected error: %v", c.machine, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("slotsFor(%q) = %v, want %v", c.machine, got, c.want)
		}
	}
}

func TestSlotsForInvalid(t *testing.T) {
	if _, err := slotsFor("c"); err == nil {
		t.Fatal("slotsFor(\"c\") expected an error, got nil")
	}
}

func TestOrHelpers(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q, want \"-\"", got)
	}
	if got := orDash("x"); got != "x" {
		t.Errorf("orDash(%q) = %q, want %q", "x", got, "x")
	}
	if got := orAbsent(""); got != "ABSENT" {
		t.Errorf("orAbsent(\"\") = %q, want \"ABSENT\"", got)
	}
	if got := orQ(""); got != "?" {
		t.Errorf("orQ(\"\") = %q, want \"?\"", got)
	}
}
