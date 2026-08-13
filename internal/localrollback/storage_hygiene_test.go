package localrollback

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSealSuppressesByteIdenticalRewriteWithRestoredMTime(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "same.txt")
	writeRollbackTestFile(t, path, "identical bytes")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := Start(stateDir, "identical-rewrite", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("identical bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for _, change := range session.Seal(time.Now()).Changes {
		if change.Path == path {
			t.Fatalf("byte-identical rewrite reported as %#v", change)
		}
	}
}

func TestSealOverReportsLargeFileWhenAutomaticComparisonIsBounded(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	contents := bytes.Repeat([]byte{'x'}, 8*1024*1024+1)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := Start(stateDir, "large-fallback", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	assertRollbackChange(t, session.Seal(time.Now()).Changes, "modified", path)
}

func TestRestoreRecognizesOriginalBytesWithoutMatchingMTime(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeRollbackTestFile(t, path, "original")
	session, _, err := Start(stateDir, "manual-original", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "session change")
	session.Seal(time.Now())
	writeRollbackTestFile(t, path, "original")
	results, err := Restore(stateDir, "manual-original", []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "already-restored" || results[0].Sidecar != "" {
		t.Fatalf("restore results = %#v, want already-restored without sidecar", results)
	}
	if matches, err := filepath.Glob(path + ".unring-*.snapshot"); err != nil || len(matches) != 0 {
		t.Fatalf("conflict sidecars = %#v, %v, want none", matches, err)
	}
}

func TestRestoreStillRefusesGenuinelyDifferentBytes(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeRollbackTestFile(t, path, "original")
	session, _, err := Start(stateDir, "real-conflict", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "changed!")
	session.Seal(time.Now())
	writeRollbackTestFile(t, path, "imposter")
	results, err := Restore(stateDir, "real-conflict", []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "refused" || results[0].Sidecar == "" {
		t.Fatalf("restore results = %#v, want refused with sidecar", results)
	}
	assertRollbackTestFile(t, results[0].Sidecar, "original")
}

func TestSealReportsMetadataChangeWhenCloneIsMissing(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "missing-clone.txt")
	writeRollbackTestFile(t, path, "same bytes")
	session, _, err := Start(stateDir, "missing-clone", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath, err := snapshotPathFor(session.manifest, session.dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "same bytes")
	assertRollbackChange(t, session.Seal(time.Now()).Changes, "modified", path)
}

func TestSealReportsMetadataChangeWhenCloneReadFails(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "unreadable-clone.txt")
	writeRollbackTestFile(t, path, "same bytes")
	session, _, err := Start(stateDir, "unreadable-clone", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath, err := snapshotPathFor(session.manifest, session.dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "same bytes")
	assertRollbackChange(t, session.Seal(time.Now()).Changes, "modified", path)
}

func TestSealReportsMetadataChangeWhenLiveFileCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "unreadable-live.txt")
	writeRollbackTestFile(t, path, "same bytes")
	session, _, err := Start(stateDir, "unreadable-live", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "same bytes")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	baseline := session.manifest.Roots[0].Before[path]
	baseline.Mode = 0
	session.manifest.Roots[0].Before[path] = baseline
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	assertRollbackChange(t, session.Seal(time.Now()).Changes, "modified", path)
}

func TestVolumeRestoreSameSizeConflictRefusesBeforeMount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume.txt")
	writeRollbackTestFile(t, path, "imposter")
	current, _, err := currentEntry(path)
	if err != nil {
		t.Fatal(err)
	}
	before := current
	before.MTime--
	after := current
	after.MTime -= 2
	platform := &fakeVolumeSnapshotPlatform{created: true, present: true, excluded: map[string]bool{}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	results, err := RestoreRecorded(t.TempDir(), "volume-preflight", Summary{Changes: []Change{{
		Kind: "modified", Path: path, Before: &before, After: &after,
		RestoreSource:  RestoreSourceVolume,
		VolumeSnapshot: &VolumeSnapshot{Name: "com.apple.TimeMachine.2026-08-09-120000.local", VolumeID: "disk-test-apfs", MountPoint: "/"},
	}}}, []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "refused" {
		t.Fatalf("results = %#v, want preflight refusal", results)
	}
	if platform.mountCalls != 0 {
		t.Fatalf("volume snapshot mounted %d times for guaranteed refusal", platform.mountCalls)
	}
}

func TestPlanRetentionCombinesAgeAndCapAndKeepsNewest(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "snapshots")
	for _, value := range []manifest{
		{Version: manifestVersion, SessionID: "old", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), StorageBytes: 80, StorageExact: true},
		{Version: manifestVersion, SessionID: "new", StartedAt: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), StorageBytes: 80, StorageExact: true},
	} {
		directory := filepath.Join(root, value.SessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(directory, value); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanRetention(stateDir, []StoredSession{
		{ID: "old", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "new", StartedAt: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)},
	}, 100, 14*24*time.Hour, time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removals) != 1 || plan.Removals[0].SessionID != "old" ||
		!plan.Removals[0].Expired || !plan.Removals[0].CapRequired {
		t.Fatalf("retention removals = %#v, want old selected once by both limits", plan.Removals)
	}
	if plan.After.Bytes != 80 || plan.After.Sessions != 1 {
		t.Fatalf("retention after = %#v, want literal 80 bytes and one newest session", plan.After)
	}
}

func TestRetentionDaysConfigDefaultsAndOverrides(t *testing.T) {
	stateDir := t.TempDir()
	days, err := RetentionDaysForState(stateDir)
	if err != nil || days != 14 {
		t.Fatalf("default retention days = %d, %v, want 14", days, err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	days, err = RetentionDaysForState(stateDir)
	if err != nil || days != 30 {
		t.Fatalf("configured retention days = %d, %v, want 30", days, err)
	}
}

func TestPlanRetentionSkipsDamagedSnapshotAndContinues(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "snapshots")
	for _, value := range []manifest{
		{Version: manifestVersion, SessionID: "healthy-old", StartedAt: time.Unix(1, 0), StorageBytes: 80, StorageExact: true},
		{Version: manifestVersion, SessionID: "healthy-new", StartedAt: time.Unix(3, 0), StorageBytes: 80, StorageExact: true},
	} {
		directory := filepath.Join(root, value.SessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(directory, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRetention(stateDir, []StoredSession{
		{ID: "healthy-old", StartedAt: time.Unix(1, 0)},
		{ID: "damaged", StartedAt: time.Unix(2, 0)},
		{ID: "healthy-new", StartedAt: time.Unix(3, 0)},
	}, 100, 365*24*time.Hour, time.Unix(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].SessionID != "damaged" {
		t.Fatalf("warnings = %#v, want damaged snapshot warning", plan.Warnings)
	}
	if len(plan.Removals) != 1 || plan.Removals[0].SessionID != "healthy-old" || !plan.Removals[0].CapRequired {
		t.Fatalf("removals = %#v, want healthy-old cap eviction", plan.Removals)
	}
}

func TestApplyRetentionReportsCompletedAndPartiallyAppliedRemovals(t *testing.T) {
	stateDir := t.TempDir()
	for _, id := range []string{"a", "b", "c"} {
		directory := filepath.Join(stateDir, "snapshots", id)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "data"), []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removals := []RetentionRemoval{
		{SessionID: "a", HasSnapshot: true},
		{SessionID: "b", HasSnapshot: true},
		{SessionID: "c", HasSnapshot: true},
	}
	var completed []string
	err := ApplyRetentionRemovals(stateDir, removals, nil, func(removal RetentionRemoval) error {
		if removal.SessionID == "c" {
			return errors.New("audit update denied")
		}
		return nil
	}, func(removal RetentionRemoval) {
		completed = append(completed, removal.SessionID)
	})
	if len(completed) != 2 || completed[0] != "a" || completed[1] != "b" {
		t.Fatalf("completed = %#v, want literal [a b]", completed)
	}
	var applyErr *RetentionApplyError
	if !errors.As(err, &applyErr) || !applyErr.SnapshotRemoved || applyErr.Removal.SessionID != "c" {
		t.Fatalf("apply error = %#v, want c with removed snapshot", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, statErr := os.Stat(filepath.Join(stateDir, "snapshots", id)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("snapshot %s remains after reported removal: %v", id, statErr)
		}
	}
}
