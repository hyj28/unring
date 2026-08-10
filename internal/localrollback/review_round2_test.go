package localrollback

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewRound2Platform struct {
	created         bool
	present         bool
	unknownMissing  bool
	excluded        map[string]bool
	isExcludedCalls []string
	snapshotFiles   map[string]string
	snapshotLinks   map[string]string
}

func (*reviewRound2Platform) Supported() (bool, string) { return true, "" }

func (*reviewRound2Platform) VolumeForPath(string) (Volume, error) {
	return Volume{ID: "review-round-2-disk", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (platform *reviewRound2Platform) IsExcluded(path string) (bool, error) {
	path = filepath.Clean(path)
	platform.isExcludedCalls = append(platform.isExcludedCalls, path)
	if platform.unknownMissing {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return false, errors.New("unexpected tmutil isexcluded output: [UNKNOWN]")
		}
	}
	return platform.excluded[path], nil
}

func (platform *reviewRound2Platform) ListSnapshots(Volume) ([]string, error) {
	if platform.created && platform.present {
		return []string{"com.apple.TimeMachine.2026-08-09-230000.local"}, nil
	}
	return nil, nil
}

func (platform *reviewRound2Platform) CreateSnapshots() (string, error) {
	platform.created = true
	platform.present = true
	return "com.apple.TimeMachine.2026-08-09-230000.local", nil
}

func (platform *reviewRound2Platform) MountSnapshot(_ VolumeSnapshot, mountPoint string) error {
	for original, target := range platform.snapshotLinks {
		destination := filepath.Join(mountPoint, strings.TrimPrefix(filepath.Clean(original), string(os.PathSeparator)))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(target, destination); err != nil {
			return err
		}
	}
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
func (*reviewRound2Platform) UnmountSnapshot(string) error { return nil }

func TestWideChangeChecksExcludedChildUnderIncludedParent(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	wideDir := filepath.Join(home, "work")
	child := filepath.Join(wideDir, "vm.sparsebundle")
	for _, path := range []string{cloneRoot, wideDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(child, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := &reviewRound2Platform{excluded: map[string]bool{child: true}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "excluded-child", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	platform.isExcludedCalls = nil
	if err := os.WriteFile(child, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(2, 0))
	change := findReviewChange(summary.Changes, child)
	if change == nil {
		t.Fatalf("excluded child change absent: %#v", summary.Changes)
	}
	if change.RestoreSource != RestoreSourceNone || !strings.Contains(change.UnrestorableReason, "excluded") {
		t.Fatalf("excluded child change = %#v, want an exclusion coverage failure", *change)
	}
	if len(platform.isExcludedCalls) != 1 || platform.isExcludedCalls[0] != child {
		t.Fatalf("IsExcluded calls = %#v, want the changed child itself", platform.isExcludedCalls)
	}
	assertRollbackFailure(t, summary.Backstop.Excluded, child, "excluded from the Time Machine backup")
}

func TestDeletedDirectoryChecksSurvivingAncestor(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	parent := filepath.Join(home, "work")
	deleted := filepath.Join(parent, "deleted")
	for _, path := range []string{cloneRoot, deleted} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	platform := &reviewRound2Platform{excluded: map[string]bool{}, unknownMissing: true}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "deleted-directory-exclusion", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	platform.isExcludedCalls = nil
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(2, 0))
	change := findReviewChange(summary.Changes, deleted)
	if change == nil {
		t.Fatalf("deleted directory change absent: %#v", summary.Changes)
	}
	if change.RestoreSource != RestoreSourceVolume || change.UnrestorableReason != "" {
		t.Fatalf("deleted directory change = %#v, want snapshot coverage inherited from surviving parent", *change)
	}
	if len(platform.isExcludedCalls) != 1 || platform.isExcludedCalls[0] != parent {
		t.Fatalf("IsExcluded calls = %#v, want surviving parent %s", platform.isExcludedCalls, parent)
	}
}

func TestExcludedChangedDirectoryShortCircuitsDescendantChecks(t *testing.T) {
	home := t.TempDir()
	stateDir := t.TempDir()
	cloneRoot := filepath.Join(home, "watched")
	work := filepath.Join(home, "work")
	excludedDirectory := filepath.Join(work, "excluded-build")
	for _, path := range []string{cloneRoot, work} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	platform := &reviewRound2Platform{excluded: map[string]bool{excludedDirectory: true}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := StartScope(stateDir, "excluded-directory-cache", Scope{
		Watched: []string{cloneRoot}, ScanRoot: home,
	}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	platform.isExcludedCalls = nil
	if err := os.Mkdir(excludedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		path := filepath.Join(excludedDirectory, "artifact-"+formatReviewIndex(index))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summary := session.Seal(time.Unix(2, 0))
	if len(summary.Changes) != 101 {
		t.Fatalf("changes = %d, want literal 101", len(summary.Changes))
	}
	if len(platform.isExcludedCalls) != 1 || platform.isExcludedCalls[0] != excludedDirectory {
		t.Fatalf("IsExcluded calls = %#v, want one excluded ancestor lookup", platform.isExcludedCalls)
	}
	for _, change := range summary.Changes {
		if change.RestoreSource != RestoreSourceNone || !strings.Contains(change.UnrestorableReason, "excluded") {
			t.Fatalf("change = %#v, want inherited exclusion", change)
		}
	}
}

func TestRestoreAllRemovesCreatedChildBeforeDirectoryMetadata(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "sub")
	created := filepath.Join(directory, "new.txt")
	if err := os.Mkdir(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	platform := &reviewRound2Platform{excluded: map[string]bool{}}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	session, _, err := Start(stateDir, "created-child-before-directory-mode", []string{root}, 1<<30, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("created"), 0o600); err != nil {
		t.Fatal(err)
	}
	summary := session.Seal(time.Unix(2, 0))
	if findReviewChange(summary.Changes, directory) == nil || findReviewChange(summary.Changes, created) == nil {
		t.Fatalf("changes = %#v, want modified directory and created child", summary.Changes)
	}
	results, err := Restore(stateDir, "created-child-before-directory-mode", []string{directory, created}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Status != "restored" {
			t.Fatalf("restore results = %#v, want both paths restored", results)
		}
	}
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created child still exists: %v", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("directory mode = %o, want literal 500", info.Mode().Perm())
	}
}

func TestVolumeRestoreReadsPhysicalSnapshotPathBehindLogicalSymlink(t *testing.T) {
	base := t.TempDir()
	stateDir := t.TempDir()
	physicalRoot := filepath.Join(base, "physical-config")
	logicalRoot := filepath.Join(base, "logical-config")
	if err := os.Mkdir(physicalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physicalRoot, logicalRoot); err != nil {
		t.Fatal(err)
	}
	physicalFile := filepath.Join(physicalRoot, "settings.json")
	logicalFile := filepath.Join(logicalRoot, "settings.json")
	if err := os.WriteFile(physicalFile, []byte("before snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _, err := currentEntry(logicalFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physicalFile, []byte("live after child"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, err := currentEntry(logicalFile)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &VolumeSnapshot{
		Name:     "com.apple.TimeMachine.2026-08-09-230000.local",
		VolumeID: "review-round-2-disk", MountPoint: "/",
	}
	platform := &reviewRound2Platform{
		created: true, present: true, excluded: map[string]bool{},
		snapshotFiles: map[string]string{physicalFile: "before snapshot"},
		snapshotLinks: map[string]string{logicalRoot: physicalRoot},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	summary := Summary{Changes: []Change{{
		Kind: "modified", Path: logicalFile, VolumeSnapshotPath: physicalFile,
		Before: &before, After: &after, RestoreSource: RestoreSourceVolume,
		VolumeSnapshot: snapshot,
	}}}
	results, err := RestoreRecorded(stateDir, "physical-snapshot-path", summary, []string{logicalFile}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "restored" {
		t.Fatalf("restore results = %#v, want one restored path", results)
	}
	contents, err := os.ReadFile(physicalFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "before snapshot" {
		t.Fatalf("restored contents = %q, want snapshot bytes rather than live bytes", contents)
	}
}
