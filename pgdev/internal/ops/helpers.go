package ops

import (
	"os"
	"path/filepath"
	"strings"
)

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func ensureDir(path string) error { return os.MkdirAll(path, 0o700) }

func pruneAt(pruneDir, name string) string { return filepath.Join(pruneDir, name) }
