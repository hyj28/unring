package localrollback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reviewRoundPlatform struct {
	created         bool
	present         bool
	isExcludedCalls []string
	batchCalls      int
}

func (*reviewRoundPlatform) Supported() (bool, string) { return true, "" }

func (*reviewRoundPlatform) VolumeForPath(string) (Volume, error) {
	return Volume{ID: "review-disk", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (platform *reviewRoundPlatform) IsExcluded(path string) (bool, error) {
	platform.isExcludedCalls = append(platform.isExcludedCalls, filepath.Clean(path))
	return false, nil
}

func (platform *reviewRoundPlatform) IsExcludedBatch(_ context.Context, paths []string) ([]bool, error) {
	platform.batchCalls++
	results := make([]bool, len(paths))
	for index, path := range paths {
		results[index], _ = platform.IsExcluded(path)
	}
	return results, nil
}

func TestWideInclusionChecksUseFarFewerBatchesThanPaths(t *testing.T) {
	platform := &reviewRoundPlatform{}
	backstop := Backstop{Available: true, Snapshots: []VolumeSnapshot{{
		Name: "literal-snapshot", VolumeID: "review-disk", MountPoint: "/",
	}}}
	changes := make([]Change, 600)
	indexes := make([]int, 600)
	for index := range changes {
		changes[index] = Change{
			Kind: "created", Path: filepath.Join("/literal/work", fmt.Sprintf("artifact-%03d", index)),
			After: &Entry{Type: "file"},
		}
		indexes[index] = index
	}
	failures := classifyWideChanges(context.Background(), changes, indexes, platform, &backstop, nil)
	if len(failures) != 0 {
		t.Fatalf("classification failures = %#v", failures)
	}
	if platform.batchCalls != 3 {
		t.Fatalf("batch calls = %d, want literal 3 for 600 paths", platform.batchCalls)
	}
	if len(platform.isExcludedCalls) != 600 {
		t.Fatalf("mapped per-path statuses = %d, want literal 600", len(platform.isExcludedCalls))
	}
}

func (platform *reviewRoundPlatform) ListSnapshots(Volume) ([]string, error) {
	if platform.created && platform.present {
		return []string{"com.apple.TimeMachine.2026-08-09-140000.local"}, nil
	}
	return nil, nil
}

func (platform *reviewRoundPlatform) CreateSnapshots() (string, error) {
	platform.created = true
	platform.present = true
	return "com.apple.TimeMachine.2026-08-09-140000.local", nil
}

func (*reviewRoundPlatform) MountSnapshot(VolumeSnapshot, string) error { return nil }
func (*reviewRoundPlatform) UnmountSnapshot(string) error               { return nil }

func TestRestoreRecordedOrdersCreatedTreeAfterCloneEviction(t *testing.T) {
	stateDir := t.TempDir()
	createdDir := filepath.Join(t.TempDir(), "newdir")
	createdFile := filepath.Join(createdDir, "a.txt")
	if err := os.Mkdir(createdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(createdFile, []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirEntry, _, err := currentEntry(createdDir)
	if err != nil {
		t.Fatal(err)
	}
	fileEntry, _, err := currentEntry(createdFile)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &VolumeSnapshot{
		Name:     "com.apple.TimeMachine.2026-08-09-140000.local",
		VolumeID: "review-disk", MountPoint: "/",
	}
	summary := Summary{Changes: []Change{
		{Kind: "created", Path: createdDir, After: &dirEntry, RestoreSource: RestoreSourceVolume, VolumeSnapshot: snapshot},
		{Kind: "created", Path: createdFile, After: &fileEntry, RestoreSource: RestoreSourceVolume, VolumeSnapshot: snapshot},
	}}
	platform := &reviewRoundPlatform{created: true, present: true}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	results, err := RestoreRecorded(
		stateDir, "clone-evicted-created-tree", summary,
		[]string{createdDir, createdFile}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Status != "restored" {
			t.Fatalf("restore result = %#v, want every created path restored", results)
		}
	}
	if _, err := os.Lstat(createdDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created directory still exists after restore --all: %v", err)
	}
}

func TestSealChecksEveryIncludedChangedPathForTimeMachineExclusion(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	wideDir := filepath.Join(home, "work", "target")
	for _, path := range []string{cloneRoot, wideDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	platform := &reviewRoundPlatform{}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "memoized-exclusions", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	platform.isExcludedCalls = nil
	for index := 0; index < 500; index++ {
		path := filepath.Join(wideDir, "artifact-"+formatReviewIndex(index))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summary := session.Seal(time.Unix(2, 0))
	if len(summary.Changes) != 500 {
		t.Fatalf("changes = %d, want 500", len(summary.Changes))
	}
	if len(platform.isExcludedCalls) != 500 {
		t.Fatalf("IsExcluded call count = %d, want one exact lookup for each of 500 included changed paths", len(platform.isExcludedCalls))
	}
}

func formatReviewIndex(index int) string {
	const digits = "0123456789"
	return string([]byte{
		digits[(index/100)%10], digits[(index/10)%10], digits[index%10],
	})
}

func TestWidenedDiffKeepsUncapturablePathInsideWatchedRoot(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	secret := filepath.Join(cloneRoot, "secret.txt")
	if err := os.MkdirAll(cloneRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("pre-session secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := &reviewRoundPlatform{}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, started, err := StartScope(stateDir, "uncapturable-wide-fallback", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
		Uncaptured: []CaptureFailure{{Path: secret, Error: "permission denied"}},
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	assertRollbackFailure(t, started.Uncaptured, secret, "permission denied")
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(2, 0))
	change := findReviewChange(summary.Changes, secret)
	if change == nil {
		t.Fatalf("uncapturable deleted path vanished from changes: %#v", summary.Changes)
	}
	if change.Kind != "deleted" || change.RestoreSource != RestoreSourceVolume {
		t.Fatalf("uncapturable path change = %#v, want snapshot-only deletion", *change)
	}
}

func findReviewChange(changes []Change, path string) *Change {
	for index := range changes {
		if filepath.Clean(changes[index].Path) == filepath.Clean(path) {
			return &changes[index]
		}
	}
	return nil
}
