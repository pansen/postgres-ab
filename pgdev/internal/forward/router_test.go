package forward

import "testing"

func TestTargets(t *testing.T) {
	const port = 5432
	tests := []struct {
		name                    string
		slot, ipA, ipB          string
		wantActive, wantStaging string
	}{
		{"a active both up", "a", "10.0.0.1", "10.0.0.2", "10.0.0.1:5432", "10.0.0.2:5432"},
		{"b active both up", "b", "10.0.0.1", "10.0.0.2", "10.0.0.2:5432", "10.0.0.1:5432"},
		{"pointer flip swaps both", "b", "1.1.1.1", "2.2.2.2", "2.2.2.2:5432", "1.1.1.1:5432"},
		{"missing active IP", "a", "", "10.0.0.2", "", "10.0.0.2:5432"},
		{"missing staging IP", "a", "10.0.0.1", "", "10.0.0.1:5432", ""},
		{"both missing", "a", "", "", "", ""},
		{"garbage slot defaults to a", "x", "10.0.0.1", "10.0.0.2", "10.0.0.1:5432", "10.0.0.2:5432"},
		{"empty slot defaults to a", "", "10.0.0.1", "10.0.0.2", "10.0.0.1:5432", "10.0.0.2:5432"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, staging := Targets(tt.slot, tt.ipA, tt.ipB, port)
			if active != tt.wantActive || staging != tt.wantStaging {
				t.Errorf("Targets(%q, %q, %q) = (%q, %q), want (%q, %q)",
					tt.slot, tt.ipA, tt.ipB, active, staging, tt.wantActive, tt.wantStaging)
			}
		})
	}
}

// A pointer flip must move BOTH client ports to the sibling machine with the
// same two IPs — the load-bearing promote behaviour.
func TestTargetsPointerFlip(t *testing.T) {
	a1, s1 := Targets("a", "10.0.0.1", "10.0.0.2", 5432)
	a2, s2 := Targets("b", "10.0.0.1", "10.0.0.2", 5432)
	if a1 != s2 || s1 != a2 {
		t.Fatalf("flip did not swap targets: before=(%q,%q) after=(%q,%q)", a1, s1, a2, s2)
	}
}
