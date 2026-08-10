package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

type cliBackstopPlatform struct {
	created       bool
	present       bool
	createErr     error
	excluded      map[string]bool
	snapshotFiles map[string]string
	onMount       func()
}

func (*cliBackstopPlatform) Supported() (bool, string) { return true, "" }

func (*cliBackstopPlatform) VolumeForPath(string) (localrollback.Volume, error) {
	return localrollback.Volume{ID: "disk-cli-apfs", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (platform *cliBackstopPlatform) IsExcluded(path string) (bool, error) {
	return platform.excluded[filepath.Clean(path)], nil
}

func (platform *cliBackstopPlatform) ListSnapshots(localrollback.Volume) ([]string, error) {
	if platform.created && platform.present {
		return []string{"com.apple.TimeMachine.2026-08-09-130000.local"}, nil
	}
	return nil, nil
}

func (platform *cliBackstopPlatform) CreateSnapshots() (string, error) {
	if platform.createErr != nil {
		return "", platform.createErr
	}
	platform.created = true
	platform.present = true
	return "com.apple.TimeMachine.2026-08-09-130000.local", nil
}

func (platform *cliBackstopPlatform) MountSnapshot(_ localrollback.VolumeSnapshot, mountPoint string) error {
	if platform.onMount != nil {
		platform.onMount()
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

func (*cliBackstopPlatform) UnmountSnapshot(string) error { return nil }

func TestCLIReportsAndRestoresWidenedSnapshotOnlyDeletion(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	cloneRoot := t.TempDir()
	outside := filepath.Join(home, "outside-clone.txt")
	if err := os.WriteFile(outside, []byte("before from APFS"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := &cliBackstopPlatform{excluded: map[string]bool{}}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()

	var runStdout, runStderr bytes.Buffer
	exitCode := Main([]string{
		"run", "--watch", cloneRoot, "--", "/bin/sh", "-c", `rm "$1"`, "unring-test", outside,
	}, strings.NewReader(""), &runStdout, &runStderr)
	if exitCode != 0 {
		t.Fatalf("run exit = %d\nstdout: %s\nstderr: %s", exitCode, runStdout.String(), runStderr.String())
	}
	if !strings.Contains(runStdout.String(), "outside-clone.txt") ||
		!strings.Contains(runStdout.String(), "SNAPSHOT ONLY: restoring this path requires sudo") {
		t.Fatalf("widened deletion was not marked snapshot-only:\n%s", runStdout.String())
	}
	if !strings.Contains(runStderr.String(), "whole-volume backstop recorded com.apple.TimeMachine.2026-08-09-130000.local") {
		t.Fatalf("start output omitted recorded snapshot:\n%s", runStderr.String())
	}

	store, err := audit.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("audit records = %d, %v", len(records), err)
	}
	var changedPath string
	for _, change := range records[0].Files.Changes {
		if filepath.Base(change.Path) == "outside-clone.txt" {
			changedPath = change.Path
		}
	}
	if changedPath == "" {
		t.Fatalf("outside deletion absent from audit: %#v", records[0].Files.Changes)
	}
	platform.snapshotFiles = map[string]string{changedPath: "before from APFS"}
	var restoreStdout, restoreStderr bytes.Buffer
	platform.onMount = func() {
		if !strings.Contains(restoreStderr.String(), "ROOT PRIVILEGES REQUIRED FOR SNAPSHOT-ONLY RESTORE") ||
			!strings.Contains(restoreStderr.String(), "macOS permits only root to mount") {
			t.Errorf("root explanation was not printed before mount attempt: %s", restoreStderr.String())
		}
	}
	exitCode = Main([]string{"restore", records[0].ID, changedPath}, strings.NewReader(""), &restoreStdout, &restoreStderr)
	if exitCode != 0 {
		t.Fatalf("restore exit = %d\nstdout: %s\nstderr: %s", exitCode, restoreStdout.String(), restoreStderr.String())
	}
	contents, err := os.ReadFile(outside)
	if err != nil || string(contents) != "before from APFS" {
		t.Fatalf("restored contents = %q, %v", contents, err)
	}

	var snapshotsStdout, snapshotsStderr bytes.Buffer
	if exitCode := Main([]string{"snapshots"}, strings.NewReader(""), &snapshotsStdout, &snapshotsStderr); exitCode != 0 {
		t.Fatalf("snapshots present exit = %d: %s", exitCode, snapshotsStderr.String())
	}
	if !strings.Contains(snapshotsStdout.String(), "com.apple.TimeMachine.2026-08-09-130000.local on / — present") {
		t.Fatalf("snapshots did not report presence:\n%s", snapshotsStdout.String())
	}
	platform.present = false
	snapshotsStdout.Reset()
	if exitCode := Main([]string{"snapshots"}, strings.NewReader(""), &snapshotsStdout, &snapshotsStderr); exitCode != 0 {
		t.Fatalf("snapshots purged exit = %d: %s", exitCode, snapshotsStderr.String())
	}
	if !strings.Contains(snapshotsStdout.String(), "PURGED OR DELETED") {
		t.Fatalf("snapshots did not report purge:\n%s", snapshotsStdout.String())
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	restoreStdout.Reset()
	restoreStderr.Reset()
	exitCode = Main([]string{"restore", records[0].ID, changedPath}, strings.NewReader(""), &restoreStdout, &restoreStderr)
	if exitCode != internalErrorExitCode {
		t.Fatalf("purged restore exit = %d, want %d", exitCode, internalErrorExitCode)
	}
	if !strings.Contains(restoreStderr.String(), "ROOT PRIVILEGES REQUIRED") ||
		!strings.Contains(restoreStderr.String(), "is no longer present; it was purged or deleted") {
		t.Fatalf("purged restore did not explain root boundary and purge:\n%s", restoreStderr.String())
	}
}

func TestCLINoTimeMachineWarningAndExcludedWatchAreProminent(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	t.Run("no Time Machine", func(t *testing.T) {
		t.Setenv("UNRING_STATE_DIR", t.TempDir())
		watched := t.TempDir()
		platform := &cliBackstopPlatform{
			excluded: map[string]bool{}, createErr: errors.New("No destinations configured"),
		}
		restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
		defer restorePlatform()
		var stdout, stderr bytes.Buffer
		exitCode := Main([]string{"run", "--watch-only", watched, "--", "/usr/bin/true"}, strings.NewReader(""), &stdout, &stderr)
		if exitCode != 0 {
			t.Fatalf("run exit = %d: %s", exitCode, stderr.String())
		}
		for _, literal := range []string{
			"NO WHOLE-VOLUME BACKSTOP",
			"No destinations configured",
			"Changes outside the clone scope can be reported, but their prior contents cannot be restored",
			"The child will still run with clone-based protection",
		} {
			if !strings.Contains(stderr.String(), literal) {
				t.Fatalf("no-Time-Machine warning omitted %q:\n%s", literal, stderr.String())
			}
		}
	})

	t.Run("excluded watched path", func(t *testing.T) {
		t.Setenv("UNRING_STATE_DIR", t.TempDir())
		watched := t.TempDir()
		platform := &cliBackstopPlatform{excluded: map[string]bool{filepath.Clean(watched): true}}
		restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
		defer restorePlatform()
		var stdout, stderr bytes.Buffer
		exitCode := Main([]string{"run", "--watch-only", watched, "--", "/usr/bin/true"}, strings.NewReader(""), &stdout, &stderr)
		if exitCode != 0 {
			t.Fatalf("run exit = %d: %s", exitCode, stderr.String())
		}
		literal := "PATH OUTSIDE WHOLE-VOLUME BACKSTOP: " + watched + ": excluded from the Time Machine backup"
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("excluded watch warning omitted literal path:\n%s", stderr.String())
		}
	})
}
