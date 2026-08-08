package localrollback

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSymlinkedWatchedRootProtectsTargetTree(t *testing.T) {
	stateDir := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "Documents")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target, "taxes.pdf")
	writeRollbackTestFile(t, file, "before")

	session, summary, err := Start(stateDir, "symlink-root", []string{link}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(summary.Uncaptured) != 0 {
		t.Fatalf("symlinked root uncaptured = %#v", summary.Uncaptured)
	}
	if len(summary.Watched) != 1 || summary.Watched[0] != link {
		t.Fatalf("watched = %#v, want logical symlink %q", summary.Watched, link)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	sealed := session.Seal(time.Now())
	logicalFile := filepath.Join(link, "taxes.pdf")
	assertRollbackChange(t, sealed.Changes, "deleted", logicalFile)
	results, err := Restore(stateDir, "symlink-root", []string{logicalFile}, false)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(results) != 1 || results[0].Status != "restored" {
		t.Fatalf("restore results = %#v", results)
	}
	assertRollbackTestFile(t, file, "before")
}

func TestSymlinkedDirectoryInsideRootIsNamedAsUncaptured(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	target := t.TempDir()
	writeRollbackTestFile(t, filepath.Join(target, "outside.txt"), "outside")
	link := filepath.Join(root, "data")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, summary, err := Start(stateDir, "nested-symlink", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRollbackFailure(t, summary.Uncaptured, link, "symlinked directory target")
}

func TestSymlinkedDirectoryIntroducedDuringSessionMakesScanIncomplete(t *testing.T) {
	root := t.TempDir()
	session, _, err := Start(t.TempDir(), "new-symlink", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	target := t.TempDir()
	link := filepath.Join(root, "late-data")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	sealed := session.Seal(time.Now())
	if sealed.Complete || !strings.Contains(sealed.Error, link) || !strings.Contains(sealed.Error, "not followed") {
		t.Fatalf("sealed summary = %#v, want named incomplete symlink coverage", sealed)
	}
}

func TestCaptureReconciliationRejectsEveryScanCloneRaceShape(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	old := Entry{Type: "file", Size: 4, MTime: 10, CTime: 20, Mode: uint32(0o600), Links: 1}
	newEntry := old
	newEntry.CTime = 21
	for _, test := range []struct {
		name     string
		before   map[string]Entry
		after    map[string]Entry
		snapshot map[string]Entry
	}{
		{name: "created", before: map[string]Entry{}, after: map[string]Entry{file: old}, snapshot: map[string]Entry{file: old}},
		{name: "modified", before: map[string]Entry{file: old}, after: map[string]Entry{file: newEntry}, snapshot: map[string]Entry{file: old}},
		{name: "deleted", before: map[string]Entry{file: old}, after: map[string]Entry{}, snapshot: map[string]Entry{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, failures := reconcileCapture(test.before, test.after, test.snapshot, nil)
			assertRollbackFailure(t, failures, file, "changed while")
		})
	}
}

func TestMissingRootAndUnsupportedObjectsAreNamedAsUncaptured(t *testing.T) {
	stateDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "Dcouments")
	root := t.TempDir()
	fifo := filepath.Join(root, "events.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	_, summary, err := Start(stateDir, "missing-special", []string{missing, root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRollbackFailure(t, summary.Uncaptured, missing, "does not exist")
	assertRollbackFailure(t, summary.Uncaptured, fifo, "unsupported file type")
}

func TestHardlinksAreExplicitlyOutsideCoverage(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeRollbackTestFile(t, first, "shared")
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	_, summary, err := Start(t.TempDir(), "hardlinks", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRollbackFailure(t, summary.Uncaptured, first, "hard-linked")
	assertRollbackFailure(t, summary.Uncaptured, second, "hard-linked")
}

func TestExactNanosecondMTimeStillNeedsCTime(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "same-size")
	writeRollbackTestFile(t, file, "before")
	originalTime := time.Unix(1_700_000_000, 123_456_789)
	if err := os.Chtimes(file, originalTime, originalTime); err != nil {
		t.Fatal(err)
	}
	session, _, err := Start(t.TempDir(), "ctime", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	writeRollbackTestFile(t, file, "after!")
	if err := os.Chtimes(file, originalTime, originalTime); err != nil {
		t.Fatal(err)
	}
	sealed := session.Seal(time.Now())
	assertRollbackChange(t, sealed.Changes, "modified", file)
	change := findRollbackChange(t, sealed.Changes, file)
	if change.Before.MTime != change.After.MTime {
		t.Fatalf("mtime differs: before=%d after=%d; test does not isolate ctime", change.Before.MTime, change.After.MTime)
	}
	if change.Before.Size != change.After.Size {
		t.Fatalf("size differs: before=%d after=%d; test does not isolate ctime", change.Before.Size, change.After.Size)
	}
	if change.Before.CTime == change.After.CTime {
		t.Fatal("ctime did not advance")
	}
}

func TestEmptyDirectoryCreationAndDeletionAreRecorded(t *testing.T) {
	root := t.TempDir()
	createdSession, _, err := Start(t.TempDir(), "directory-created", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(root, "empty-created")
	if err := os.Mkdir(created, 0o751); err != nil {
		t.Fatal(err)
	}
	assertRollbackChange(t, createdSession.Seal(time.Now()).Changes, "created", created)

	deleted := filepath.Join(root, "empty-deleted")
	if err := os.Mkdir(deleted, 0o751); err != nil {
		t.Fatal(err)
	}
	deletedSession, _, err := Start(t.TempDir(), "directory-deleted", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	assertRollbackChange(t, deletedSession.Seal(time.Now()).Changes, "deleted", deleted)
}

func TestRestoreRecreatesParentsWithModesAndIsIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	public := filepath.Join(root, "public")
	assets := filepath.Join(public, "assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(assets, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(assets, "logo.txt")
	writeRollbackTestFile(t, file, "logo-before")
	session, _, err := Start(stateDir, "parents", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(public); err != nil {
		t.Fatal(err)
	}
	session.Seal(time.Now())
	for attempt := 0; attempt < 2; attempt++ {
		results, err := Restore(stateDir, "parents", []string{file}, false)
		if err != nil {
			t.Fatalf("restore attempt %d: %v", attempt+1, err)
		}
		wantStatus := "restored"
		if attempt == 1 {
			wantStatus = "already-restored"
		}
		if len(results) != 1 || results[0].Status != wantStatus {
			t.Fatalf("attempt %d results = %#v, want %s", attempt+1, results, wantStatus)
		}
	}
	assertRollbackTestFile(t, file, "logo-before")
	assertRollbackMode(t, public, 0o751)
	assertRollbackMode(t, assets, 0o750)
}

func TestRestorableModePreservesSpecialPermissionBits(t *testing.T) {
	want := os.FileMode(0o751) | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if got := restorableMode(uint32(want | os.ModeIrregular)); got != want {
		t.Fatalf("restorable mode = %v, want %v", got, want)
	}
}

func TestRestoreRefusesIncompleteAndEvictedSnapshots(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeRollbackTestFile(t, file, "before")
	incomplete, _, err := Start(stateDir, "incomplete", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, file, "after!")
	if _, err := Restore(stateDir, "incomplete", []string{file}, false); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("restore incomplete error = %v", err)
	}
	incomplete.Seal(time.Now())
	if _, _, err := EnforceRetention(stateDir, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(stateDir, "incomplete", []string{file}, false); err == nil || !strings.Contains(err.Error(), "evicted") {
		t.Fatalf("restore evicted error = %v", err)
	}
}

func TestStorageUsageUsesLiteralMeasuredBytesWithoutLogicalFallback(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "snapshots")
	for _, value := range []manifest{
		{Version: manifestVersion, SessionID: "one", StartedAt: time.Unix(1, 0), StorageBytes: 111, StorageExact: true, LogicalBytes: 9000},
		{Version: manifestVersion, SessionID: "two", StartedAt: time.Unix(2, 0), StorageBytes: 0, StorageExact: true, LogicalBytes: 8000},
	} {
		directory := filepath.Join(root, value.SessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(directory, value); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := StorageUsage(stateDir, 777)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 111 || usage.CapBytes != 777 || usage.Sessions != 2 || !usage.Exact {
		t.Fatalf("usage = %#v, want literal 111 bytes of 777 across two exact sessions", usage)
	}
	if err := SaveRetentionCap(stateDir, 54321); err != nil {
		t.Fatal(err)
	}
	capBytes, err := RetentionCapForState(stateDir)
	if err != nil || capBytes != 54321 {
		t.Fatalf("stored cap = %d, %v, want 54321", capBytes, err)
	}
}

func TestEvictionWaitsForRestoreReaderLock(t *testing.T) {
	stateDir := t.TempDir()
	directory := filepath.Join(stateDir, "snapshots", "locked")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(directory, manifest{
		Version: manifestVersion, SessionID: "locked", StartedAt: time.Unix(1, 0),
		StorageBytes: 100, StorageExact: true,
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireSnapshotLock(stateDir, "locked", unix.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := EnforceRetention(stateDir, 0, "")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("eviction completed while reader lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists after reader released: %v", err)
	}
}

func TestRetentionEvictsSnapshotContainingReadOnlyTree(t *testing.T) {
	stateDir := t.TempDir()
	directory := filepath.Join(stateDir, "snapshots", "read-only")
	packageDirectory := filepath.Join(directory, "roots", "000000", "go", "pkg", "mod", "example@v1.0.0")
	if err := os.MkdirAll(packageDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDirectory, "source.go"), []byte("package example\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(directory, manifest{
		Version: manifestVersion, SessionID: "read-only", StartedAt: time.Unix(1, 0),
		StorageBytes: 100, StorageExact: true,
	}); err != nil {
		t.Fatal(err)
	}
	for current := packageDirectory; current != directory; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o555); err != nil {
			t.Fatal(err)
		}
	}

	evicted, usage, err := EnforceRetention(stateDir, 0, "")
	if err != nil {
		t.Fatalf("evict read-only snapshot: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != "read-only" {
		t.Fatalf("evicted = %#v, want [read-only]", evicted)
	}
	if usage.Bytes != 0 || usage.Sessions != 0 {
		t.Fatalf("usage after eviction = %#v, want zero bytes and sessions", usage)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only snapshot remains after eviction: %v", err)
	}
}

func TestRetentionReportsSnapshotItCannotEvict(t *testing.T) {
	stateDir := t.TempDir()
	snapshotRoot := filepath.Join(stateDir, "snapshots")
	directory := filepath.Join(snapshotRoot, "blocked")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(directory, manifest{
		Version: manifestVersion, SessionID: "blocked", StartedAt: time.Unix(1, 0),
		StorageBytes: 100, StorageExact: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snapshotRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapshotRoot, 0o700) })

	_, _, err := EnforceRetention(stateDir, 0, "")
	if err == nil || !strings.Contains(err.Error(), "evict snapshot blocked") {
		t.Fatalf("retention error = %v, want named eviction failure", err)
	}
}

func writeRollbackTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRollbackTestFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertRollbackFailure(t *testing.T, failures []CaptureFailure, path, message string) {
	t.Helper()
	for _, failure := range failures {
		if failure.Path == path && strings.Contains(failure.Error, message) {
			return
		}
	}
	t.Fatalf("failures = %#v, want %s containing %q", failures, path, message)
}

func assertRollbackChange(t *testing.T, changes []Change, kind, path string) {
	t.Helper()
	change := findRollbackChange(t, changes, path)
	if change.Kind != kind {
		t.Fatalf("change %s kind = %s, want %s", path, change.Kind, kind)
	}
}

func findRollbackChange(t *testing.T, changes []Change, path string) Change {
	t.Helper()
	for _, change := range changes {
		if change.Path == path {
			return change
		}
	}
	t.Fatalf("changes = %#v, missing %s", changes, path)
	return Change{}
}

func assertRollbackMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}
