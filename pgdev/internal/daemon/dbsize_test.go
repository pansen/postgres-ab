package daemon

import "testing"

func TestParseDBSize(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int64
	}{
		{"bare", "12345\n", 12345},
		{"padded", "  12345  \n\n", 12345},
		// `su - postgres` runs a login shell, which can print before psql does.
		{"login noise", "Last login: Sun Aug 17 21:47:14 2026\n12345\n", 12345},
	}
	for _, c := range cases {
		got, err := parseDBSize(c.out)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: parseDBSize = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseDBSizeRejectsNonNumeric(t *testing.T) {
	for _, out := range []string{"", "  \n ", "ERROR:  database \"vpg\" does not exist\n"} {
		if n, err := parseDBSize(out); err == nil {
			t.Errorf("parseDBSize(%q) = %d, want an error", out, n)
		}
	}
}
