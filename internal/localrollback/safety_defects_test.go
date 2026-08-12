package localrollback

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreMountsOneSnapshotOnceForSeveralPaths(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	first, second := filepath.Join(root, "first.txt"), filepath.Join(root, "second.txt")
	changes := []Change{
		volumeModifiedChange(t, first, "first before", "first after"),
		volumeModifiedChange(t, second, "second before", "second after"),
	}
	platform := &fakeVolumeSnapshotPlatform{
		created: true, present: true, excluded: map[string]bool{},
		snapshotFiles: map[string]string{first: "first before", second: "second before"},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	writeSealedRestoreManifest(t, stateDir, "one-mounted-batch", changes)

	results, err := Restore(stateDir, "one-mounted-batch", []string{first, second}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != "restored" || results[1].Status != "restored" {
		t.Fatalf("restore results = %#v, want two restored paths", results)
	}
	if platform.mountCalls != 1 || platform.unmountCalls != 1 {
		t.Fatalf("mount/unmount calls = %d/%d, want literal 1/1", platform.mountCalls, platform.unmountCalls)
	}
}

func TestRestoreRecordedContinuesAfterMountedPathFailureAndUnmounts(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	broken, good := filepath.Join(root, "a-broken.txt"), filepath.Join(root, "z-good.txt")
	changes := []Change{
		volumeModifiedChange(t, broken, "broken before", "broken after"),
		volumeModifiedChange(t, good, "good before", "good after"),
	}
	platform := &fakeVolumeSnapshotPlatform{
		created: true, present: true, excluded: map[string]bool{},
		snapshotFiles: map[string]string{good: "good before"},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()

	results, err := RestoreRecorded(stateDir, "continue-after-error", Summary{Changes: changes}, []string{broken, good}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != "error" || results[1].Status != "restored" {
		t.Fatalf("restore results = %#v, want error then restored", results)
	}
	if platform.mountCalls != 1 || platform.unmountCalls != 1 {
		t.Fatalf("mount/unmount calls = %d/%d, want literal 1/1", platform.mountCalls, platform.unmountCalls)
	}
	contents, err := os.ReadFile(good)
	if err != nil || string(contents) != "good before" {
		t.Fatalf("remaining path contents = %q, %v; want literal good before", contents, err)
	}
}

func TestRestoreRecordedCreatedVolumePathDoesNotMount(t *testing.T) {
	platform := &fakeVolumeSnapshotPlatform{created: true, present: true, excluded: map[string]bool{}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	path := filepath.Join(t.TempDir(), "created.txt")
	if err := os.WriteFile(path, []byte("created during session"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, err := currentEntry(path)
	if err != nil {
		t.Fatal(err)
	}
	change := Change{
		Kind: "created", Path: path, After: &after, RestoreSource: RestoreSourceVolume,
		VolumeSnapshot: &VolumeSnapshot{
			Name:     "com.apple.TimeMachine.2026-08-09-120000.local",
			VolumeID: "disk-test-apfs", MountPoint: "/",
		},
	}

	results, err := RestoreRecorded(t.TempDir(), "created-volume-selection", Summary{Changes: []Change{change}}, []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "restored" {
		t.Fatalf("restore results = %#v, want one restored created path", results)
	}
	if platform.mountCalls != 0 || platform.unmountCalls != 0 {
		t.Fatalf("mount/unmount calls = %d/%d, want literal 0/0", platform.mountCalls, platform.unmountCalls)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created volume path remains after restore: %v", err)
	}
}

func TestCompletenessIsInformationalOnlyForExactUnsupportedFailures(t *testing.T) {
	unsupported := CaptureFailure{Path: "/tmp/agent.sock", Error: UnsupportedFileTypeCoverageReason}
	unsupportedError := "post-session coverage incomplete: /tmp/agent.sock: unsupported file type is outside snapshot coverage"
	if !HasOnlyUnsupportedFileTypeFailures(Summary{
		Complete: false, Uncaptured: []CaptureFailure{unsupported}, Error: unsupportedError,
	}) {
		t.Fatal("exact unsupported-only failure was not classified as informational")
	}
	for _, summary := range []Summary{
		{Complete: false, Uncaptured: []CaptureFailure{unsupported}, Error: unsupportedError + "; write manifest: no space left on device"},
		{Complete: false, Error: "measure retained snapshot: input/output error"},
		{Complete: false, Uncaptured: []CaptureFailure{unsupported}, ScanFailures: []CaptureFailure{{Path: "/tmp", Error: "permission denied"}}, Error: unsupportedError},
	} {
		if HasOnlyUnsupportedFileTypeFailures(summary) {
			t.Fatalf("actionable incomplete summary was classified informational: %#v", summary)
		}
	}
}

func TestAgentStateRootsResolveSymlinkedHome(t *testing.T) {
	physicalHome := t.TempDir()
	logicalHome := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(physicalHome, logicalHome); err != nil {
		t.Fatal(err)
	}
	resolvedHome, err := filepath.EvalSymlinks(physicalHome)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedHome, ".claude")
	roots := AgentStateRoots(logicalHome)
	found := false
	for _, root := range roots {
		if root == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved agent roots = %#v, want root %q", roots, want)
	}
	if !IsAgentStatePathWithin(filepath.Join(want, "settings.json"), roots) {
		t.Fatal("resolved agent-state path was not recognized")
	}
}

func TestResolveScopeRejectsMissingExplicitWatchButSkipsMissingDefault(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	project := filepath.Join(base, "project")
	for _, path := range []string{home, project} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(base, "missing-explicit")
	_, err := ResolveScope(ScopeOptions{
		StateDir: t.TempDir(), WorkingDirectory: project, HomeDirectory: home,
		Watch: []string{missing},
	})
	if err == nil || !IsScopeConfigError(err) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing explicit watch error = %v, want user-input error naming path", err)
	}
	failures := ScopePreflightFailures(err)
	if len(failures) != 1 || failures[0].Path != missing || failures[0].Error != "watched path does not exist" {
		t.Fatalf("preflight failures = %#v, want one literal missing-path failure", failures)
	}

	scope, err := ResolveScope(ScopeOptions{
		StateDir: t.TempDir(), WorkingDirectory: project, HomeDirectory: home,
	})
	if err != nil {
		t.Fatalf("missing defaults must remain silent: %v", err)
	}
	for _, watched := range scope.Watched {
		if strings.HasPrefix(watched, home+string(os.PathSeparator)) {
			t.Fatalf("missing default unexpectedly watched: %s", watched)
		}
	}
}

func volumeModifiedChange(t *testing.T, path, beforeContents, afterContents string) Change {
	t.Helper()
	if err := os.WriteFile(path, []byte(beforeContents), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _, err := currentEntry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(afterContents), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, err := currentEntry(path)
	if err != nil {
		t.Fatal(err)
	}
	return Change{
		Kind: "modified", Path: path, Before: &before, After: &after,
		RestoreSource: RestoreSourceVolume,
		VolumeSnapshot: &VolumeSnapshot{
			Name:     "com.apple.TimeMachine.2026-08-09-120000.local",
			VolumeID: "disk-test-apfs", MountPoint: "/",
		},
	}
}

func writeSealedRestoreManifest(t *testing.T, stateDir, sessionID string, changes []Change) {
	t.Helper()
	directory := filepath.Join(stateDir, "snapshots", sessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(directory, manifest{
		Version: manifestVersion, SessionID: sessionID,
		StartedAt: time.Unix(1, 0), EndedAt: time.Unix(2, 0), Changes: changes,
	}); err != nil {
		t.Fatal(err)
	}
}
