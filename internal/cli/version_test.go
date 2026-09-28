package cli

import "strings"

import "testing"

func TestInjectedVersionIsPreferredOverBuildInfo(t *testing.T) {
	original := Version
	defer func() { Version = original }()

	Version = "v9.9.9-injected"
	injected := versionString()
	if !strings.HasPrefix(injected, "unring v9.9.9-injected") {
		t.Fatalf("injected version = %q, want the literal injected value first", injected)
	}

	Version = ""
	fallback := versionString()
	if strings.Contains(fallback, "v9.9.9-injected") {
		t.Fatalf("fallback version leaked the injected value: %q", fallback)
	}
	if !strings.HasPrefix(fallback, "unring ") {
		t.Fatalf("fallback version = %q, want it to still name the program", fallback)
	}
}
