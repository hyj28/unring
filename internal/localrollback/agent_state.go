package localrollback

import (
	"os"
	"path/filepath"
	"strings"
)

// agentStateRelativeRoots is the declared list of known agent-owned state.
// Keep this list explicit: grouping these paths must never depend on a name
// heuristic. The same list is documented in README.md.
var agentStateRelativeRoots = []string{
	".claude",
	".codex",
	filepath.Join(".config", "opencode"),
	filepath.Join(".local", "share", "opencode"),
	filepath.Join(".cache", "opencode"),
}

// AgentStateRoots returns the declared absolute roots treated as agent-owned
// state. An empty home uses the current user's home directory.
func AgentStateRoots(home string) []string {
	if home == "" {
		home = os.Getenv("HOME")
		if home == "" {
			resolved, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			home = resolved
		}
	}
	roots := make([]string, 0, len(agentStateRelativeRoots)*2)
	seen := make(map[string]bool)
	for _, relative := range agentStateRelativeRoots {
		root := filepath.Clean(filepath.Join(home, relative))
		for _, candidate := range []string{root, resolvedAgentStateRoot(root)} {
			if candidate != "" && !seen[candidate] {
				seen[candidate] = true
				roots = append(roots, candidate)
			}
		}
	}
	return roots
}

func resolvedAgentStateRoot(root string) string {
	resolved, err := resolvePathAllowMissing(root)
	if err != nil {
		return ""
	}
	return resolved
}

// IsAgentStatePath reports whether path is within a declared agent-state root.
func IsAgentStatePath(path, home string) bool {
	return IsAgentStatePathWithin(path, AgentStateRoots(home))
}

// IsAgentStatePathWithin reports whether path is within one of the persisted
// resolved agent-state roots.
func IsAgentStatePathWithin(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true
		}
	}
	// Older records may contain only physical roots, so retain a physical-path
	// comparison as a compatibility fallback.
	if resolved, err := resolvePathAllowMissing(path); err == nil && resolved != path {
		for _, root := range roots {
			if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
				return true
			}
		}
	}
	return false
}
