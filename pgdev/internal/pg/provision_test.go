package pg

import "testing"

func TestSQLEscaping(t *testing.T) {
	if got := sqlLit("o'brien"); got != "'o''brien'" {
		t.Fatalf("sqlLit = %q", got)
	}
	if got := ident(`we"ird`); got != `"we""ird"` {
		t.Fatalf("ident = %q", got)
	}
}

// A password containing a single quote must not break out of the SQL literal.
func TestRoleDBScriptEscapesPassword(t *testing.T) {
	s := RoleDBScript("app", "db", "pa'ss")
	if !contains(s, "PASSWORD 'pa''ss'") {
		t.Fatalf("password not escaped in:\n%s", s)
	}
	if !contains(s, `CREATE ROLE "app"`) || !contains(s, `CREATE DATABASE "db" OWNER "app"`) {
		t.Fatalf("role/db statements wrong:\n%s", s)
	}
}

func TestGoldenBuildScriptInstallsAndSheds(t *testing.T) {
	s := GoldenBuildScript()
	for _, want := range []string{
		"apt-get install -y --no-install-recommends postgresql-17",
		"RequiresMountsFor=/var/lib/postgresql",
		"pg_dropcluster --stop 17 main",
	} {
		if !contains(s, want) {
			t.Fatalf("golden build script missing %q", want)
		}
	}
}

func TestClusterScriptCreatesAndConfigures(t *testing.T) {
	s := ClusterScript()
	for _, want := range []string{
		"pg_createcluster 17 main",
		"conf.d/99-dev.conf",
		"fsync = off",
		"systemctl restart postgresql@17-main",
		"chmod 0755 /var/lib/postgresql",
	} {
		if !contains(s, want) {
			t.Fatalf("cluster script missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
