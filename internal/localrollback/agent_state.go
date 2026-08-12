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
	roots := make([]string, 0, len(agentStateRelativeRoots))
	for _, relative := range agentStateRelativeRoots {
		roots = append(roots, filepath.Clean(filepath.Join(home, relative)))
	}
	return roots
}

// IsAgentStatePath reports whether path is within a declared agent-state root.
func IsAgentStatePath(path, home string) bool {
	path = filepath.Clean(path)
	for _, root := range AgentStateRoots(home) {
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
