package localrollback

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadSummaryRestoresPersistedChangeListScope(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	session, _, err := StartScope(stateDir, "stored-scope-2026", Scope{
		Watched:         []string{root},
		ChangeListScope: ChangeListScopeWatchOnly,
		ChangeListRoots: []string{root},
	}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatalf("StartScope() error: %v", err)
	}
	session.Seal(time.Unix(20, 0))

	summary, err := LoadSummary(stateDir, "stored-scope")
	if err != nil {
		t.Fatalf("LoadSummary() error: %v", err)
	}
	if summary.ChangeListScope != "watch-only" {
		t.Fatalf("change-list scope = %q, want literal watch-only", summary.ChangeListScope)
	}
	if !reflect.DeepEqual(summary.ChangeListRoots, []string{root}) {
		t.Fatalf("change-list roots = %#v, want %#v", summary.ChangeListRoots, []string{root})
	}
	if !summary.Retained {
		t.Fatal("loaded retained manifest was reported as evicted")
	}
}

func TestLoadSummaryDoesNotInventScopeForLegacyManifest(t *testing.T) {
	stateDir := t.TempDir()
	directory := filepath.Join(stateDir, "snapshots", "legacy-session")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(directory, manifest{
		Version: manifestVersion, SessionID: "legacy-session",
		Roots: []rootManifest{{Path: "/literal/legacy/root"}},
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := LoadSummary(stateDir, "legacy-session")
	if err != nil {
		t.Fatalf("LoadSummary() error: %v", err)
	}
	if summary.ChangeListScope != "" || len(summary.ChangeListRoots) != 0 {
		t.Fatalf("legacy scope = %q, roots %#v; want no claim", summary.ChangeListScope, summary.ChangeListRoots)
	}
}
