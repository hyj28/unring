package localrollback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirectorySelectionExpandsRecordedChangesWithoutDuplicates(t *testing.T) {
	stateDir, root, directory, file, summary := captureDeletedDirectory(t, "directory-selection")
	_ = stateDir
	_ = root

	selected, err := ChangesForRestore(summary.Changes, []string{directory, file})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		directory:                              true,
		file:                                   true,
		filepath.Join(directory, "b"):          true,
		filepath.Join(directory, "b", "y.txt"): true,
	}
	if len(selected) != len(want) {
		t.Fatalf("selected changes = %#v, want four independently named paths", selected)
	}
	for _, change := range selected {
		if !want[change.Path] {
			t.Fatalf("selected unexpected path %s", change.Path)
		}
		delete(want, change.Path)
	}
	if len(want) != 0 {
		t.Fatalf("selection omitted paths: %#v", want)
	}

	single, err := ChangesForRestore(summary.Changes, []string{file})
	if err != nil || len(single) != 1 || single[0].Path != file {
		t.Fatalf("single-file selection = %#v, %v; want the one named file", single, err)
	}
}

func TestRestoreNamedDirectoryRestoresDescendantsAndProtectsEachConflict(t *testing.T) {
	stateDir, _, directory, conflict, _ := captureDeletedDirectory(t, "directory-conflict")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, conflict, "later user bytes")

	results, err := Restore(stateDir, "directory-conflict", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 4)
	byPath := restoreResultsByPath(results)
	if byPath[conflict].Status != "refused" || byPath[conflict].Sidecar == "" {
		t.Fatalf("conflicting descendant result = %#v, want refused with sidecar", byPath[conflict])
	}
	assertRollbackTestFile(t, conflict, "later user bytes")
	assertRollbackTestFile(t, byPath[conflict].Sidecar, "literal x before\n")
	assertRollbackTestFile(t, filepath.Join(directory, "b", "y.txt"), "literal y before\n")
	if byPath[filepath.Join(directory, "b")].Status != "restored" {
		t.Fatalf("fresh nested directory result = %#v, want restored", byPath[filepath.Join(directory, "b")])
	}

	results, err = Restore(stateDir, "directory-conflict", []string{directory}, true)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 4)
	if got := restoreResultsByPath(results)[conflict].Status; got != "restored" {
		t.Fatalf("forced conflicting descendant status = %q, want restored", got)
	}
	assertRollbackTestFile(t, conflict, "literal x before\n")
}

func TestRestoreFreshDirectoryReportsDirectoriesRestored(t *testing.T) {
	stateDir, _, directory, _, _ := captureDeletedDirectory(t, "fresh-directory-status")
	results, err := Restore(stateDir, "fresh-directory-status", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := restoreResultsByPath(results)
	for _, path := range []string{directory, filepath.Join(directory, "b")} {
		if byPath[path].Status != "restored" {
			t.Fatalf("fresh directory result for %s = %#v, want restored", path, byPath[path])
		}
	}
}

func TestRestoreDirectoryReplacedByFileFailsWithoutClaimingDirectoryRestored(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(directory, "child.txt")
	writeRollbackTestFile(t, child, "literal child baseline\n")
	session, _, err := StartScope(stateDir, "directory-replaced-by-file", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, directory, "literal replacement file\n")
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 2 {
		t.Fatalf("replacement fixture = %#v, want directory modification and child deletion", summary)
	}

	results, err := Restore(stateDir, "directory-replaced-by-file", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := restoreResultsByPath(results)
	if byPath[directory].Status != "error" || byPath[directory].Err == nil ||
		!strings.Contains(byPath[directory].Err.Error(), "existing path has type") {
		t.Fatalf("replacement directory result = %#v, want a type error", byPath[directory])
	}
	if byPath[directory].Status == "restored" || byPath[directory].Status == "already-restored" {
		t.Fatalf("replacement directory was falsely reported restored: %#v", byPath[directory])
	}
	assertRollbackTestFile(t, directory, "literal replacement file\n")
}

func TestRestoreRecordedReadOnlyDirectoryWithMultipleChildren(t *testing.T) {
	stateDir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil && entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	root := t.TempDir()
	directory := filepath.Join(root, "read-only")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(directory, "first.txt")
	second := filepath.Join(directory, "second.txt")
	writeRollbackTestFile(t, first, "literal first baseline\n")
	writeRollbackTestFile(t, second, "literal second baseline\n")
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "read-only-directory", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 3 {
		t.Fatalf("read-only fixture = %#v, want directory and two deleted files", summary)
	}

	results, err := Restore(stateDir, "read-only-directory", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 3)
	for _, result := range results {
		if result.Status != "restored" {
			t.Fatalf("read-only restore result = %#v, want restored", result)
		}
	}
	assertRollbackTestFile(t, first, "literal first baseline\n")
	assertRollbackTestFile(t, second, "literal second baseline\n")
	assertRollbackMode(t, directory, 0o555)
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreChildrenBeforeRevertingExistingDirectoryToWritableMode(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "mode-changed")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(directory, "first.txt")
	second := filepath.Join(directory, "second.txt")
	writeRollbackTestFile(t, first, "literal first before\n")
	writeRollbackTestFile(t, second, "literal second before\n")
	session, _, err := StartScope(stateDir, "existing-read-only-directory", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, first, "literal first after\n")
	writeRollbackTestFile(t, second, "literal second after\n")
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 3 {
		t.Fatalf("existing read-only fixture = %#v, want directory and two modified files", summary)
	}

	results, err := Restore(stateDir, "existing-read-only-directory", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 3)
	for _, result := range results {
		if result.Status != "restored" {
			t.Fatalf("existing read-only restore result = %#v, want restored", result)
		}
	}
	assertRollbackTestFile(t, first, "literal first before\n")
	assertRollbackTestFile(t, second, "literal second before\n")
	assertRollbackMode(t, directory, 0o700)
}

func TestRestoreNamedFileRestoresUnselectedReadOnlyAncestorMode(t *testing.T) {
	stateDir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil && entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	root := t.TempDir()
	directory := filepath.Join(root, "read-only-ancestor")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "child.txt")
	writeRollbackTestFile(t, file, "literal unselected ancestor baseline\n")
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
	session, _, err := StartScope(stateDir, "unselected-read-only-ancestor", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, file, "literal unselected ancestor session bytes\n")
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 1 || summary.Changes[0].Path != file {
		t.Fatalf("read-only ancestor fixture = %#v, want one independently observed file change", summary)
	}

	results, err := Restore(stateDir, "unselected-read-only-ancestor", []string{file}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != file || results[0].Status != "restored" {
		t.Fatalf("named-file results = %#v, want one restored file", results)
	}
	assertRollbackTestFile(t, file, "literal unselected ancestor baseline\n")
	assertRollbackMode(t, directory, 0o555)
}

func TestDirectoryMetadataFailureHasItsOwnResult(t *testing.T) {
	stateDir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil && entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	root := t.TempDir()
	directory := filepath.Join(root, "metadata-failure")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "child.txt")
	writeRollbackTestFile(t, file, "literal metadata failure baseline\n")
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "metadata-failure-result", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, file, "literal metadata failure session bytes\n")
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 1 || summary.Changes[0].Path != file {
		t.Fatalf("metadata failure fixture = %#v, want one independently observed file change", summary)
	}

	var bytesBeforeFailure string
	restoreHook := SetRestoreMetadataHookForTest(func(path string) {
		if path != directory {
			return
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Errorf("read child before metadata failure: %v", readErr)
			return
		}
		bytesBeforeFailure = string(data)
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			t.Errorf("remove directory before metadata application: %v", removeErr)
		}
	})
	defer restoreHook()
	results, err := Restore(stateDir, "metadata-failure-result", []string{file}, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytesBeforeFailure != "literal metadata failure baseline\n" {
		t.Fatalf("child bytes before metadata failure = %q, want literal baseline", bytesBeforeFailure)
	}
	byPath := restoreResultsByPath(results)
	if byPath[file].Status != "restored" || byPath[file].Err != nil {
		t.Fatalf("restored child result was rewritten: %#v", byPath[file])
	}
	if byPath[directory].Status != "error" || byPath[directory].Err == nil ||
		!strings.Contains(byPath[directory].Err.Error(), "apply restored directory metadata") {
		t.Fatalf("directory metadata result = %#v, want its own error", byPath[directory])
	}
}

func TestRestoreNamedUnchangedDirectoryTouchesOnlyChangedDescendants(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(directory, "x.txt")
	writeRollbackTestFile(t, changed, "literal baseline\n")
	session, _, err := StartScope(stateDir, "directory-with-new-file", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, changed, "session bytes\n")
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 1 || summary.Changes[0].Path != changed {
		t.Fatalf("fixture changes = %#v, want one independently verified changed file", summary.Changes)
	}
	newPath := filepath.Join(directory, "new.txt")
	writeRollbackTestFile(t, newPath, "later unrecorded bytes\n")

	results, err := Restore(stateDir, "directory-with-new-file", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != changed {
		t.Fatalf("directory results = %#v, want the one recorded changed descendant", results)
	}
	assertRollbackTestFile(t, changed, "literal baseline\n")
	assertRollbackTestFile(t, newPath, "later unrecorded bytes\n")
}

func TestRestoreRecordedExpandsDirectoryAfterCloneEviction(t *testing.T) {
	stateDir, _, directory, _, summary := captureDeletedDirectory(t, "directory-recorded")
	if err := os.RemoveAll(filepath.Join(stateDir, "snapshots", "directory-recorded")); err != nil {
		t.Fatal(err)
	}
	results, err := RestoreRecorded(stateDir, "directory-recorded", summary, []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 4)
	for _, result := range results {
		if result.Status != "unavailable" {
			t.Fatalf("evicted descendant result = %#v, want unavailable", result)
		}
	}
}

func TestRestoreNamedParentSkipsAgentStateDescendantsUnlessExplicit(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "scope")
	agentDirectory := filepath.Join(directory, ".agent-state")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(directory, "regular.txt")
	agentFile := filepath.Join(agentDirectory, "state.db")
	writeRollbackTestFile(t, regular, "literal regular baseline\n")
	writeRollbackTestFile(t, agentFile, "literal agent baseline\n")
	session, _, err := StartScope(stateDir, "directory-agent-state", Scope{
		Watched: []string{root}, AgentStateRoots: []string{agentDirectory},
	}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 4 {
		t.Fatalf("agent-state fixture = %#v, want four recorded deletions", summary)
	}

	results, err := Restore(stateDir, "directory-agent-state", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := restoreResultsByPath(results)
	if byPath[agentDirectory].Status != "skipped" || byPath[agentFile].Status != "skipped" {
		t.Fatalf("implicit agent-state results = %#v, want both skipped", results)
	}
	assertRollbackTestFile(t, regular, "literal regular baseline\n")
	if _, err := os.Lstat(agentFile); !os.IsNotExist(err) {
		t.Fatalf("implicitly selected agent-state file exists: %v", err)
	}

	results, err = Restore(stateDir, "directory-agent-state", []string{agentDirectory}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("explicit agent-state results = %#v, want directory and file", results)
	}
	assertRollbackTestFile(t, agentFile, "literal agent baseline\n")
}

func captureDeletedDirectory(t *testing.T, sessionID string) (string, string, string, string, Summary) {
	t.Helper()
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "x.txt")
	writeRollbackTestFile(t, file, "literal x before\n")
	writeRollbackTestFile(t, filepath.Join(nested, "y.txt"), "literal y before\n")
	session, _, err := StartScope(stateDir, sessionID, Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(20, 0))
	if !summary.Complete || len(summary.Changes) != 4 {
		t.Fatalf("fixture summary = %#v, want complete literal four-path deletion", summary)
	}
	return stateDir, root, directory, file, summary
}

func restoreResultsByPath(results []RestoreResult) map[string]RestoreResult {
	byPath := make(map[string]RestoreResult, len(results))
	for _, result := range results {
		byPath[result.Path] = result
	}
	return byPath
}

func assertUniqueRestorePaths(t *testing.T, results []RestoreResult, want int) {
	t.Helper()
	seen := make(map[string]bool)
	for _, result := range results {
		if seen[result.Path] {
			t.Fatalf("path %s reported twice in %#v", result.Path, results)
		}
		seen[result.Path] = true
	}
	if len(results) != want {
		t.Fatalf("restore results = %#v, want %d paths", results, want)
	}
}
