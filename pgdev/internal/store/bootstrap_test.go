package store

import "testing"

func TestParseIEC(t *testing.T) {
	cases := map[string]int64{
		"140G": 140 << 30,
		"180G": 180 << 30,
		"1024M": 1024 << 20,
		"512K": 512 << 10,
		"2T":   2 << 40,
		"1000": 1000,
	}
	for in, want := range cases {
		got, err := parseIEC(in)
		if err != nil {
			t.Fatalf("parseIEC(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("parseIEC(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := parseIEC("notasize"); err == nil {
		t.Fatal("expected error for garbage size")
	}
}

// RemoveSlotData must reject anything but a/b so a bug can't rm -rf outside a slot.
func TestRemoveSlotDataGuardsSlot(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if err := s.RemoveSlotData("../etc"); err == nil {
		t.Fatal("expected rejection of non-slot path")
	}
	if err := s.RemoveSlotData("a"); err != nil {
		t.Fatalf("slot a should be allowed: %v", err)
	}
}
