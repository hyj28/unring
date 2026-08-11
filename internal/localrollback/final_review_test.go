package localrollback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVolumeRestoreRerootsDataVolumeAbsoluteSnapshotSymlink(t *testing.T) {
	base := t.TempDir()
	stateDir := t.TempDir()
	physicalRoot := filepath.Join(base, "physical")
	logicalRoot := filepath.Join(base, "logical")
	if err := os.Mkdir(physicalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physicalRoot, logicalRoot); err != nil {
		t.Fatal(err)
	}
	physicalFile := filepath.Join(physicalRoot, "creds.json")
	logicalFile := filepath.Join(logicalRoot, "creds.json")
	if err := os.WriteFile(physicalFile, []byte("pre-session data-volume bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _, err := currentEntry(logicalFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physicalFile, []byte("post-session live bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, err := currentEntry(logicalFile)
	if err != nil {
		t.Fatal(err)
	}
	const dataVolume = "/System/Volumes/Data"
	snapshotTarget := filepath.Join(dataVolume, strings.TrimPrefix(physicalRoot, string(os.PathSeparator)))
	snapshot := &VolumeSnapshot{
		Name:     "com.apple.TimeMachine.2026-08-09-230000.local",
		VolumeID: "review-round-2-disk", MountPoint: dataVolume,
	}
	platform := &reviewRound2Platform{
		created: true, present: true, excluded: map[string]bool{},
		snapshotFiles: map[string]string{physicalFile: "pre-session data-volume bytes"},
		snapshotLinks: map[string]string{logicalRoot: snapshotTarget},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	summary := Summary{Changes: []Change{{
		Kind: "modified", Path: logicalFile, Before: &before, After: &after,
		RestoreSource: RestoreSourceVolume, VolumeSnapshot: snapshot,
	}}}
	results, err := RestoreRecorded(stateDir, "data-volume-absolute-link", summary, []string{logicalFile}, false)
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
	if string(contents) != "pre-session data-volume bytes" {
		t.Fatalf("restored contents = %q, want literal pre-session data-volume bytes", contents)
	}
}

func TestVolumeRestoreRefusesRelativeSnapshotSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	stateDir := t.TempDir()
	liveRoot := filepath.Join(base, "live")
	liveFile := filepath.Join(liveRoot, "secrets.txt")
	if err := os.Mkdir(liveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveFile, []byte("pre-session bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _, err := currentEntry(liveFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveFile, []byte("post-session bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, _, err := currentEntry(liveFile)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &VolumeSnapshot{
		Name:     "com.apple.TimeMachine.2026-08-09-230000.local",
		VolumeID: "review-round-2-disk", MountPoint: "/",
	}
	platform := &reviewRound2Platform{
		created: true, present: true, excluded: map[string]bool{},
		snapshotLinks: map[string]string{
			liveRoot: strings.Repeat("../", 64) + "outside-the-mounted-snapshot",
		},
	}
	restorePlatform := SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	summary := Summary{Changes: []Change{{
		Kind: "modified", Path: liveFile, Before: &before, After: &after,
		RestoreSource: RestoreSourceVolume, VolumeSnapshot: snapshot,
	}}}
	results, err := RestoreRecorded(stateDir, "relative-link-escape", summary, []string{liveFile}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "error" || results[0].Err == nil ||
		!strings.Contains(results[0].Err.Error(), "mounted snapshot symlink escaped its root") {
		t.Fatalf("restore results = %#v, want a loud symlink-escape error", results)
	}
	if results[0].Status == "restored" {
		t.Fatalf("snapshot escape was reported restored: %#v", results[0])
	}
	contents, err := os.ReadFile(liveFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "post-session bytes" {
		t.Fatalf("live contents = %q, want unchanged literal post-session bytes", contents)
	}
}
