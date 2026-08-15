package localrollback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewRoundPlatform struct {
	created         bool
	present         bool
	isExcludedCalls []string
	batchCalls      int
	batchPaths      [][]string
	excluded        map[string]bool
	batchErr        error
	singleErr       error
}

func (*reviewRoundPlatform) Supported() (bool, string) { return true, "" }

func (*reviewRoundPlatform) VolumeForPath(string) (Volume, error) {
	return Volume{ID: "review-disk", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (platform *reviewRoundPlatform) IsExcluded(path string) (bool, error) {
	platform.isExcludedCalls = append(platform.isExcludedCalls, filepath.Clean(path))
	return platform.excluded[filepath.Clean(path)], platform.singleErr
}

func (platform *reviewRoundPlatform) IsExcludedBatch(_ context.Context, paths []string) ([]bool, error) {
	platform.batchCalls++
	platform.batchPaths = append(platform.batchPaths, append([]string(nil), paths...))
	if len(paths) > 1 && platform.batchErr != nil {
		return nil, platform.batchErr
	}
	results := make([]bool, len(paths))
	for index, path := range paths {
		if len(paths) == 1 {
			platform.isExcludedCalls = append(platform.isExcludedCalls, filepath.Clean(path))
		}
		results[index] = platform.excluded[filepath.Clean(path)]
	}
	if len(paths) == 1 && platform.singleErr != nil {
		return nil, platform.singleErr
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
	if len(platform.isExcludedCalls) != 0 {
		t.Fatalf("serial fallback calls = %d, want literal 0 after valid batches", len(platform.isExcludedCalls))
	}
}

func TestWideInclusionBatchFailureRetriesAndAttributesEachPath(t *testing.T) {
	paths := []string{"/literal/a.txt", "/literal/b.txt", "/literal/c.txt"}
	changes := make([]Change, len(paths))
	indexes := make([]int, len(paths))
	for index, path := range paths {
		changes[index] = Change{Kind: "created", Path: path, After: &Entry{Type: "file"}}
		indexes[index] = index
	}
	backstop := Backstop{Available: true, Snapshots: []VolumeSnapshot{{
		Name: "literal-snapshot", VolumeID: "review-disk", MountPoint: "/",
	}}}
	t.Run("single retries succeed", func(t *testing.T) {
		platform := &reviewRoundPlatform{
			batchErr: errors.New("literal batch diagnostic"),
			excluded: map[string]bool{"/literal/b.txt": true},
		}
		failures := classifyWideChanges(context.Background(), changes, indexes, platform, &backstop, nil)
		if len(failures) != 0 {
			t.Fatalf("fallback failures = %#v, want none", failures)
		}
		if platform.batchCalls != 4 || len(platform.isExcludedCalls) != 3 {
			t.Fatalf("batch calls = %d, single retries = %d, want independent literals 4 and 3", platform.batchCalls, len(platform.isExcludedCalls))
		}
	})
	t.Run("single retries fail independently", func(t *testing.T) {
		failedChanges := append([]Change(nil), changes...)
		platform := &reviewRoundPlatform{
			batchErr:  errors.New("literal batch diagnostic"),
			singleErr: errors.New("literal single failure"),
		}
		failures := classifyWideChanges(context.Background(), failedChanges, indexes, platform, &backstop, nil)
		if len(failures) != 3 {
			t.Fatalf("fallback failures = %#v, want independent literal 3", failures)
		}
		for index, path := range paths {
			if failures[index].Path != path || !strings.Contains(failures[index].Error, "literal single failure") {
				t.Fatalf("failure %d = %#v, want path %q and literal single error", index, failures[index], path)
			}
		}
	})
}

func TestWideInclusionChecksUnsafePathsIndividually(t *testing.T) {
	paths := []string{
		"/literal/normal-a.txt", "/literal/normal-b.txt",
		"/literal/draft .txt", "/literal/line\nbreak.txt",
	}
	changes := make([]Change, len(paths))
	indexes := make([]int, len(paths))
	for index, path := range paths {
		changes[index] = Change{Kind: "created", Path: path, After: &Entry{Type: "file"}}
		indexes[index] = index
	}
	platform := &reviewRoundPlatform{}
	backstop := Backstop{Available: true, Snapshots: []VolumeSnapshot{{Name: "literal", VolumeID: "review-disk", MountPoint: "/"}}}
	if failures := classifyWideChanges(context.Background(), changes, indexes, platform, &backstop, nil); len(failures) != 0 {
		t.Fatalf("classification failures = %#v", failures)
	}
	if platform.batchCalls != 3 || len(platform.isExcludedCalls) != 2 {
		t.Fatalf("calls = %d batches/%d singles, want independent literals 3/2", platform.batchCalls, len(platform.isExcludedCalls))
	}
	var safeBatch []string
	for _, batch := range platform.batchPaths {
		if len(batch) == 2 {
			safeBatch = batch
		}
	}
	if len(safeBatch) != 2 || safeBatch[0] != "/literal/normal-a.txt" || safeBatch[1] != "/literal/normal-b.txt" {
		t.Fatalf("safe batch = %#v, want two independent normal paths", safeBatch)
	}
}

func TestWideInclusionDepthOrderingOnlyInheritsExcludedAncestors(t *testing.T) {
	paths := []string{
		"/literal/excluded", "/literal/excluded/deep/child.txt",
		"/literal/included", "/literal/included/deep/child.txt",
	}
	changes := make([]Change, len(paths))
	indexes := make([]int, len(paths))
	for index, path := range paths {
		changes[index] = Change{Kind: "created", Path: path, After: &Entry{Type: "file"}}
		indexes[index] = index
	}
	platform := &reviewRoundPlatform{excluded: map[string]bool{"/literal/excluded": true}}
	backstop := Backstop{Available: true, Snapshots: []VolumeSnapshot{{Name: "literal", VolumeID: "review-disk", MountPoint: "/"}}}
	if failures := classifyWideChanges(context.Background(), changes, indexes, platform, &backstop, nil); len(failures) != 0 {
		t.Fatalf("classification failures = %#v", failures)
	}
	checked := strings.Join(flattenReviewBatches(platform.batchPaths), "\n")
	if strings.Contains(checked, "/literal/excluded/deep/child.txt") {
		t.Fatalf("excluded descendant was redundantly checked:\n%s", checked)
	}
	for _, literal := range []string{"/literal/excluded", "/literal/included", "/literal/included/deep/child.txt"} {
		if !strings.Contains(checked, literal) {
			t.Fatalf("depth-ordered checks omitted %q:\n%s", literal, checked)
		}
	}
}

func flattenReviewBatches(batches [][]string) []string {
	var paths []string
	for _, batch := range batches {
		paths = append(paths, batch...)
	}
	return paths
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
	if got := len(flattenReviewBatches(platform.batchPaths)); got != 500 {
		t.Fatalf("paths submitted for inclusion = %d, want independent literal 500", got)
	}
	if platform.batchCalls != 2 || len(platform.isExcludedCalls) != 0 {
		t.Fatalf("classification used %d batches and %d serial fallbacks, want independent literals 2 and 0", platform.batchCalls, len(platform.isExcludedCalls))
	}
}

func TestCanceledFilesystemWalkPublishesInterruptedManifestWithoutUncapturedSnapshotClaim(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "literal.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "canceled-filesystem-walk", Scope{
		Watched: []string{root},
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(2, 0), nil)
	if summary.Complete || !summary.Interrupted {
		t.Fatalf("canceled walk summary = %#v, want incomplete interrupted", summary)
	}
	for _, failure := range summary.Uncaptured {
		if failure.Path == root || strings.Contains(failure.Error, context.Canceled.Error()) {
			t.Fatalf("canceled post-session walk was mislabeled as uncaptured snapshot: %#v", summary.Uncaptured)
		}
	}
	if len(summary.PostSessionFailures) != 1 || summary.PostSessionFailures[0].Path != root || summary.PostSessionFailures[0].Error != context.Canceled.Error() {
		t.Fatalf("post-session failures = %#v, want independent canceled-walk literal", summary.PostSessionFailures)
	}
	stored, err := LoadSealedSummary(stateDir, "canceled-filesystem-walk")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Complete || !stored.Interrupted {
		t.Fatalf("first published sealed manifest lost interruption: %#v", stored)
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
