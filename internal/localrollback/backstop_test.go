package localrollback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveScopeDefinesLiteralWidenedHomeScan(t *testing.T) {
	home := t.TempDir()
	watch := t.TempDir()
	for _, path := range []string{filepath.Join(home, "Library"), filepath.Join(home, "go", "pkg")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := ResolveScope(ScopeOptions{
		StateDir: t.TempDir(), WorkingDirectory: t.TempDir(), HomeDirectory: home,
		Watch: []string{watch},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if scope.ScanRoot != resolvedHome {
		t.Fatalf("scan root = %q, want resolved home %q", scope.ScanRoot, resolvedHome)
	}
	if !reflect.DeepEqual(scope.ScanExcludedNames, []string{"node_modules", ".git", ".cache"}) {
		t.Fatalf("named scan exclusions = %#v", scope.ScanExcludedNames)
	}
	wantExact := map[string]bool{}
	for _, path := range []string{filepath.Join(home, "Library"), filepath.Join(home, "go", "pkg")} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		wantExact[resolved] = true
	}
	for _, path := range scope.ScanExcluded {
		delete(wantExact, path)
	}
	if len(wantExact) != 0 {
		t.Fatalf("missing literal scan exclusions: %#v; got %#v", wantExact, scope.ScanExcluded)
	}
}

func TestResolveScopeWatchOnlyUsesCloneDiffWithoutSeparateHomeScan(t *testing.T) {
	home := t.TempDir()
	watch := t.TempDir()
	scope, err := ResolveScope(ScopeOptions{
		StateDir: t.TempDir(), WorkingDirectory: t.TempDir(), HomeDirectory: home,
		WatchOnly: []string{watch},
	})
	if err != nil {
		t.Fatal(err)
	}
	if scope.ScanRoot != "" || len(scope.ScanExcluded) != 0 || len(scope.ScanExcludedNames) != 0 {
		t.Fatalf("watch-only separate scan = %#v, want disabled; clone diff covers replacement roots", scope)
	}
}

type fakeVolumeSnapshotPlatform struct {
	supported     bool
	reason        string
	created       bool
	present       bool
	excluded      map[string]bool
	createErr     error
	mountCalls    int
	unmountCalls  int
	snapshotFiles map[string]string
	lastMounted   VolumeSnapshot
}

func (platform *fakeVolumeSnapshotPlatform) Supported() (bool, string) {
	return platform.supported, platform.reason
}

func (*fakeVolumeSnapshotPlatform) VolumeForPath(string) (Volume, error) {
	return Volume{ID: "disk-test-apfs", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (platform *fakeVolumeSnapshotPlatform) IsExcluded(path string) (bool, error) {
	return platform.excluded[filepath.Clean(path)], nil
}

func (platform *fakeVolumeSnapshotPlatform) IsExcludedBatch(_ context.Context, paths []string) ([]bool, error) {
	results := make([]bool, len(paths))
	for index, path := range paths {
		results[index] = platform.excluded[filepath.Clean(path)]
	}
	return results, nil
}

func (platform *fakeVolumeSnapshotPlatform) ListSnapshots(Volume) ([]string, error) {
	if platform.created && platform.present {
		return []string{"com.apple.TimeMachine.2026-08-09-120000.local"}, nil
	}
	return nil, nil
}

func (platform *fakeVolumeSnapshotPlatform) CreateSnapshots() (string, error) {
	if platform.createErr != nil {
		return "", platform.createErr
	}
	platform.created = true
	platform.present = true
	return "com.apple.TimeMachine.2026-08-09-120000.local", nil
}

func (platform *fakeVolumeSnapshotPlatform) MountSnapshot(snapshot VolumeSnapshot, mountPoint string) error {
	platform.mountCalls++
	platform.lastMounted = snapshot
	for original, contents := range platform.snapshotFiles {
		destination := filepath.Join(mountPoint, strings.TrimPrefix(filepath.Clean(original), string(os.PathSeparator)))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(destination, []byte(contents), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (platform *fakeVolumeSnapshotPlatform) UnmountSnapshot(string) error {
	platform.unmountCalls++
	return nil
}

func TestStartScopeRecordsBackstopAndWidenedSnapshotOnlyDeletion(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	if err := os.Mkdir(cloneRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clonePath := filepath.Join(cloneRoot, "clone.txt")
	widePath := filepath.Join(home, "outside-clone.txt")
	writeRollbackTestFile(t, clonePath, "clone before")
	writeRollbackTestFile(t, widePath, "wide before")
	excludedPaths := []string{
		filepath.Join(home, "Library", "library.txt"),
		filepath.Join(home, "project", "node_modules", "module.txt"),
		filepath.Join(home, "project", ".git", "index"),
		filepath.Join(home, ".cache", "cache.txt"),
		filepath.Join(home, "go", "pkg", "module-cache.txt"),
	}
	for _, path := range excludedPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeRollbackTestFile(t, path, "excluded before")
	}

	platform := &fakeVolumeSnapshotPlatform{supported: true, excluded: map[string]bool{}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	scope := Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
		ScanExcluded:      []string{filepath.Join(home, "Library"), filepath.Join(home, "go", "pkg")},
		ScanExcludedNames: []string{"node_modules", ".git", ".cache"},
	}
	session, started, err := StartScope(stateDir, "session-wide", scope, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("start scoped snapshot: %v", err)
	}
	if !started.Backstop.Available || len(started.Backstop.Snapshots) != 1 {
		t.Fatalf("backstop = %#v, want one available volume snapshot", started.Backstop)
	}
	if got := started.Backstop.Snapshots[0].Name; got != "com.apple.TimeMachine.2026-08-09-120000.local" {
		t.Fatalf("recorded snapshot = %q, want literal Time Machine snapshot name", got)
	}
	writeRollbackTestFile(t, clonePath, "clone after")
	if err := os.Remove(widePath); err != nil {
		t.Fatal(err)
	}
	for _, path := range excludedPaths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	summary := session.Seal(time.Unix(2, 0))
	cloneChange := findRollbackChange(t, summary.Changes, clonePath)
	if cloneChange.RestoreSource != RestoreSourceClone {
		t.Fatalf("clone change restore source = %q, want %q", cloneChange.RestoreSource, RestoreSourceClone)
	}
	wideChange := findRollbackChange(t, summary.Changes, widePath)
	if wideChange.Kind != "deleted" || wideChange.RestoreSource != RestoreSourceVolume {
		t.Fatalf("wide change = %#v, want deleted snapshot-only change", wideChange)
	}
	if wideChange.VolumeSnapshot == nil || wideChange.VolumeSnapshot.Name != "com.apple.TimeMachine.2026-08-09-120000.local" {
		t.Fatalf("wide change volume snapshot = %#v", wideChange.VolumeSnapshot)
	}
	for _, excludedPath := range excludedPaths {
		for _, change := range summary.Changes {
			if change.Path == excludedPath {
				t.Fatalf("excluded scan path was reported as changed: %s", excludedPath)
			}
		}
	}
}

func TestStartScopeReportsExcludedWatchedPathWithoutBlockingClone(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	watched := filepath.Join(home, "excluded-watch")
	if err := os.Mkdir(watched, 0o700); err != nil {
		t.Fatal(err)
	}
	platform := &fakeVolumeSnapshotPlatform{
		supported: true,
		excluded:  map[string]bool{filepath.Clean(watched): true},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, summary, err := StartScope(stateDir, "session-excluded", Scope{
		Watched: []string{watched}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("start with Time Machine exclusion: %v", err)
	}
	if session == nil {
		t.Fatal("session was not started")
	}
	assertRollbackFailure(t, summary.Backstop.Excluded, watched, "excluded from the Time Machine backup")
}

func TestVolumeRestoreChecksPurgeBeforeMountAndRestoresWhenPresent(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	if err := os.Mkdir(cloneRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	widePath := filepath.Join(home, "deleted.txt")
	writeRollbackTestFile(t, widePath, "literal snapshot contents")
	platform := &fakeVolumeSnapshotPlatform{
		supported: true, excluded: map[string]bool{},
		snapshotFiles: map[string]string{widePath: "literal snapshot contents"},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "session-restore", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(widePath); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(2, 0))

	platform.present = false
	results, err := Restore(stateDir, "session-restore", []string{widePath}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "unavailable" ||
		results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "purged or deleted") {
		t.Fatalf("purged restore result = %#v", results)
	}
	if platform.mountCalls != 0 {
		t.Fatalf("purged snapshot mount calls = %d, want 0", platform.mountCalls)
	}

	platform.present = true
	results, err = Restore(stateDir, "session-restore", []string{widePath}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "restored" {
		t.Fatalf("present snapshot restore result = %#v", results)
	}
	assertRollbackTestFile(t, widePath, "literal snapshot contents")
	if platform.mountCalls != 1 || platform.unmountCalls != 1 {
		t.Fatalf("mount/unmount calls = %d/%d, want 1/1", platform.mountCalls, platform.unmountCalls)
	}
	results, err = Restore(stateDir, "session-restore", []string{widePath}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "already-restored" {
		t.Fatalf("idempotent snapshot restore result = %#v", results)
	}
	if err := os.Remove(widePath); err != nil {
		t.Fatal(err)
	}
	if err := removeSnapshotTree(filepath.Join(stateDir, "snapshots", "session-restore")); err != nil {
		t.Fatal(err)
	}
	results, err = RestoreRecorded(stateDir, "session-restore", summary, []string{widePath}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "restored" {
		t.Fatalf("volume restore after clone eviction = %#v", results)
	}
}

func TestNoTimeMachineIsSupportedWithoutBackstop(t *testing.T) {
	root := t.TempDir()
	platform := &fakeVolumeSnapshotPlatform{
		supported: true, excluded: map[string]bool{},
		createErr: errors.New("No destinations configured"),
	}
	backstop := takeBackstop([]string{root}, platform)
	if backstop.Available {
		t.Fatalf("backstop unexpectedly available: %#v", backstop)
	}
	if !strings.Contains(backstop.Reason, "No destinations configured") {
		t.Fatalf("backstop reason = %q", backstop.Reason)
	}
}

func TestWidenedRestoreSaysWhenNoSnapshotWasEverTaken(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	if err := os.Mkdir(cloneRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside.txt")
	writeRollbackTestFile(t, outside, "before")
	platform := &fakeVolumeSnapshotPlatform{
		supported: true, excluded: map[string]bool{},
		createErr: errors.New("No destinations configured"),
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "session-never-taken", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	session.Seal(time.Unix(2, 0))
	results, err := Restore(stateDir, "session-never-taken", []string{outside}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "unavailable" || results[0].Err == nil ||
		!strings.Contains(results[0].Err.Error(), "no whole-volume snapshot was taken") {
		t.Fatalf("never-taken restore result = %#v", results)
	}
}
