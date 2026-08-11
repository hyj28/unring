package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFabricatedHomePreservesGoCaches(t *testing.T) {
	home := os.Getenv("HOME")
	for _, name := range []string{"GOCACHE", "GOMODCACHE"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Errorf("%s is empty after TestMain fabricated HOME", name)
			continue
		}
		relative, err := filepath.Rel(home, value)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			t.Errorf("%s = %s, unexpectedly beneath fabricated HOME %s", name, value, home)
		}
	}
}
