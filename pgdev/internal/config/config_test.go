package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	repo := t.TempDir()
	clearPGEnv(t)
	t.Setenv("PG_REPO_ROOT", repo)

	c := Load()
	if c.ActivePort != 5432 || c.StagingPort != 5433 {
		t.Fatalf("machine ports = %d/%d", c.ActivePort, c.StagingPort)
	}
	if c.ClientActivePort != 5442 || c.ClientStagingPort != 5443 {
		t.Fatalf("client ports = %d/%d", c.ClientActivePort, c.ClientStagingPort)
	}
	if c.ProxyHostname != "host.docker.internal" || c.BackendPrefix != "pg-dev" || c.ProxyName != "pg-proxy" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if c.AgentPort != 5440 {
		t.Fatalf("agent port = %d", c.AgentPort)
	}
	if c.AgentTokenPath != filepath.Join(repo, "var", "agent-token") {
		t.Fatalf("token path = %q", c.AgentTokenPath)
	}
}

func TestLoadReadsDotEnvButEnvWins(t *testing.T) {
	repo := t.TempDir()
	clearPGEnv(t)
	dotenv := "PG_USER=fromfile\nPG_DB=db\n# comment\nexport PG_ACTIVE_PORT=6000\nPG_PASSWORD=\"quoted\"\n"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(dotenv), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_REPO_ROOT", repo)
	t.Setenv("PG_USER", "fromenv") // environment overrides the file

	c := Load()
	if c.PGUser != "fromenv" {
		t.Fatalf("PGUser = %q, want env to win", c.PGUser)
	}
	if c.PGDB != "db" {
		t.Fatalf("PGDB = %q, want file value", c.PGDB)
	}
	if c.PGPassword != "quoted" {
		t.Fatalf("PGPassword = %q, want unquoted", c.PGPassword)
	}
	if c.ActivePort != 6000 {
		t.Fatalf("ActivePort = %d, want 6000 from `export` line", c.ActivePort)
	}
}

func TestTwoMachineDefaults(t *testing.T) {
	repo := t.TempDir()
	clearPGEnv(t)
	t.Setenv("PG_REPO_ROOT", repo)

	c := Load()
	if c.MachinePrefix != "vpg" {
		t.Fatalf("MachinePrefix = %q, want vpg", c.MachinePrefix)
	}
	if c.MachineNameForSlot("a") != "vpg-a" || c.MachineNameForSlot("b") != "vpg-b" {
		t.Fatalf("machine names = %q/%q", c.MachineNameForSlot("a"), c.MachineNameForSlot("b"))
	}
	if c.BackendPort != 5432 {
		t.Fatalf("BackendPort = %d", c.BackendPort)
	}
	if c.MachineCPUs != 4 || c.MachineMemory != "8G" {
		t.Fatalf("machine resources = %d/%q", c.MachineCPUs, c.MachineMemory)
	}
	if c.Slot != "" {
		t.Fatalf("Slot host-side = %q, want empty", c.Slot)
	}
	if c.ActiveMachinePath() != filepath.Join(repo, "var", "active-machine") {
		t.Fatalf("active-machine path = %q", c.ActiveMachinePath())
	}
	if c.MachineIPPath("b") != filepath.Join(repo, "var", "machine-ip-b") {
		t.Fatalf("machine-ip path = %q", c.MachineIPPath("b"))
	}
}

func TestMachinePrefixFromEnvAndSlot(t *testing.T) {
	repo := t.TempDir()
	clearPGEnv(t)
	t.Setenv("PG_REPO_ROOT", repo)
	t.Setenv("MACHINE_PREFIX", "test")
	t.Setenv("PG_SLOT", "b")

	c := Load()
	if c.MachineNameForSlot("b") != "test-b" {
		t.Fatalf("machine name = %q", c.MachineNameForSlot("b"))
	}
	if c.Slot != "b" {
		t.Fatalf("Slot = %q, want b (daemon-side)", c.Slot)
	}
}

func TestContainerAndSlot(t *testing.T) {
	c := Config{BackendPrefix: "pg-dev"}
	if c.Container("a") != "pg-dev-a" {
		t.Fatal(c.Container("a"))
	}
	slot, err := c.SlotForContainer("pg-dev-b")
	if err != nil || slot != "b" {
		t.Fatalf("slot=%q err=%v", slot, err)
	}
	if _, err := c.SlotForContainer("nope"); err == nil {
		t.Fatal("expected error for unknown container")
	}
}

func TestRequireCreds(t *testing.T) {
	if err := (Config{}).RequireCreds(); err == nil {
		t.Fatal("expected missing-creds error")
	}
	if err := (Config{PGUser: "a", PGDB: "b", PGPassword: "c"}).RequireCreds(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

// clearPGEnv removes inherited PG_*/MACHINE_* vars so the test controls the
// environment (the CI/dev shell may have .env exported).
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		for _, pre := range []string{"PG_", "MACHINE_", "HOST_"} {
			if len(kv) >= len(pre) && kv[:len(pre)] == pre {
				k := kv
				if i := indexByte(kv, '='); i >= 0 {
					k = kv[:i]
				}
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
		}
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
