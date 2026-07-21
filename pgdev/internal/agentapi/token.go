package agentapi

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// ReadToken reads the bearer token from path, trimming whitespace. It returns
// "" (no error) when the file is absent — the daemon then rejects every request
// until `pgdev agent deploy` writes the token and restarts it.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// EnsureToken reads path, or generates a fresh 256-bit token (0600) when it is
// absent or empty. The host calls this so both sides share one secret over the
// home-mount; the daemon only ReadTokens.
func EnsureToken(path string) (string, error) {
	if tok, err := ReadToken(path); err == nil && tok != "" {
		return tok, nil
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
