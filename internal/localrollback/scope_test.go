package localrollback

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveScopeAppliesPrecedenceAndSkipsMissingDefaults(t *testing.T) {
	stateDir := t.TempDir()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	project := filepath.Join(base, "project")
	configWatch := filepath.Join(home, "Pictures")
	flagWatch := filepath.Join(base, "z-flag-watch")
	for _, path := range []string{project, configWatch, flagWatch, filepath.Join(project, "private")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "watch:\n  - ~/Pictures\nexclude:\n  - " + filepath.Join(project, "private") + "\n"
	if err := os.WriteFile(ScopeConfigPath(stateDir), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	scope, err := ResolveScope(ScopeOptions{
		StateDir: stateDir, WorkingDirectory: project, HomeDirectory: home,
		Watch: []string{flagWatch},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWatched := []string{configWatch, project, flagWatch}
	if !reflect.DeepEqual(scope.Watched, wantWatched) {
		t.Fatalf("watched = %#v, want %#v", scope.Watched, wantWatched)
	}
	wantExcluded := []string{resolvedTestPath(t, filepath.Join(project, "private"))}
	if !reflect.DeepEqual(scope.Excluded, wantExcluded) {
		t.Fatalf("excluded = %#v, want %#v", scope.Excluded, wantExcluded)
	}
}

func TestResolveScopeWatchOnlyDiscardsDefaultsAndConfigWatch(t *testing.T) {
	stateDir := t.TempDir()
	home := t.TempDir()
	project := t.TempDir()
	only := filepath.Join(t.TempDir(), "only")
	configWatch := filepath.Join(t.TempDir(), "configured")
	excluded := filepath.Join(only, "private")
	for _, path := range []string{only, configWatch, excluded} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "watch:\n  - " + configWatch + "\nexclude:\n  - " + excluded + "\n"
	if err := os.WriteFile(ScopeConfigPath(stateDir), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	scope, err := ResolveScope(ScopeOptions{
		StateDir: stateDir, WorkingDirectory: project, HomeDirectory: home,
		WatchOnly: []string{only},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Watched, []string{only}) {
		t.Fatalf("watched = %#v, want only %q", scope.Watched, only)
	}
	if !reflect.DeepEqual(scope.Excluded, []string{resolvedTestPath(t, excluded)}) {
		t.Fatalf("excluded = %#v, want %q", scope.Excluded, excluded)
	}
}

func TestResolveScopeRejectsRelativeConfigPath(t *testing.T) {
	stateDir := t.TempDir()
	filename := ScopeConfigPath(stateDir)
	if err := os.WriteFile(filename, []byte("watch:\n  - relative/path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveScope(ScopeOptions{
		StateDir: stateDir, WorkingDirectory: t.TempDir(), HomeDirectory: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), filename) || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative config error = %v, want filename and absolute-path error", err)
	}
}

func TestExcludedSymlinkedSubtreeIsNeitherCapturedNorReported(t *testing.T) {
	stateDir := t.TempDir()
	physical := t.TempDir()
	logical := filepath.Join(t.TempDir(), "watched")
	if err := os.Symlink(physical, logical); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(physical, "included.txt")
	excluded := filepath.Join(physical, "private", "excluded.txt")
	if err := os.Mkdir(filepath.Dir(excluded), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, included, "included")
	writeRollbackTestFile(t, excluded, "excluded")
	config := "exclude:\n  - " + filepath.Join(logical, "private") + "\n"
	if err := os.WriteFile(ScopeConfigPath(stateDir), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	scope, err := ResolveScope(ScopeOptions{
		StateDir: stateDir, WorkingDirectory: t.TempDir(), HomeDirectory: t.TempDir(),
		WatchOnly: []string{logical},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantExclusion := resolvedTestPath(t, filepath.Join(physical, "private"))
	if !reflect.DeepEqual(scope.Excluded, []string{wantExclusion}) {
		t.Fatalf("resolved exclusions = %#v, want %#v", scope.Excluded, []string{wantExclusion})
	}
	session, _, err := StartWithExclusions(
		stateDir, "symlink-exclude", scope.Watched, scope.Excluded, DefaultRetentionBytes, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(included); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(excluded); err != nil {
		t.Fatal(err)
	}
	sealed := session.Seal(time.Now())
	assertRollbackChange(t, sealed.Changes, "deleted", filepath.Join(logical, "included.txt"))
	for _, change := range sealed.Changes {
		if change.Path == filepath.Join(logical, "private", "excluded.txt") {
			t.Fatalf("excluded path was reported as changed: %#v", change)
		}
	}
}

func TestAdditiveSymlinkedRootInsideDefaultIsCapturedWithoutFalseGap(t *testing.T) {
	stateDir := t.TempDir()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	logical := filepath.Join(project, "linked-data")
	if err := os.Symlink(target, logical); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target, "protected.txt")
	writeRollbackTestFile(t, file, "protected")
	scope, err := ResolveScope(ScopeOptions{
		StateDir: stateDir, WorkingDirectory: project, HomeDirectory: t.TempDir(),
		Watch: []string{logical},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope.Watched, []string{project, logical}) {
		t.Fatalf("watched = %#v, want default project and explicit symlink root", scope.Watched)
	}
	session, summary, err := StartWithExclusions(
		stateDir, "additive-symlink", scope.Watched, scope.Excluded, DefaultRetentionBytes, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uncaptured) != 0 {
		t.Fatalf("explicitly covered nested symlink was reported uncaptured: %#v", summary.Uncaptured)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	sealed := session.Seal(time.Now())
	assertRollbackChange(t, sealed.Changes, "deleted", filepath.Join(logical, "protected.txt"))
}

func resolvedTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
