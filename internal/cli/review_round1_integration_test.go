package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

type reviewRoundCLIPlatform struct {
	created   bool
	present   bool
	mountErr  error
	snapshots map[string]string
}

func (*reviewRoundCLIPlatform) Supported() (bool, string) { return true, "" }

func (*reviewRoundCLIPlatform) VolumeForPath(string) (localrollback.Volume, error) {
	return localrollback.Volume{ID: "review-cli-disk", MountPoint: "/", Filesystem: "apfs"}, nil
}

func (*reviewRoundCLIPlatform) IsExcluded(string) (bool, error) { return false, nil }

func (platform *reviewRoundCLIPlatform) ListSnapshots(localrollback.Volume) ([]string, error) {
	if platform.created && platform.present {
		return []string{"com.apple.TimeMachine.2026-08-09-150000.local"}, nil
	}
	return nil, nil
}

func (platform *reviewRoundCLIPlatform) CreateSnapshots() (string, error) {
	platform.created = true
	platform.present = true
	return "com.apple.TimeMachine.2026-08-09-150000.local", nil
}

func (platform *reviewRoundCLIPlatform) MountSnapshot(_ localrollback.VolumeSnapshot, mountPoint string) error {
	if platform.mountErr != nil {
		return platform.mountErr
	}
	for original, contents := range platform.snapshots {
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

func (*reviewRoundCLIPlatform) UnmountSnapshot(string) error { return nil }

func TestCLIWatchOnlyDisclosesUnreportedWholeVolumeChanges(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	watched := filepath.Join(home, "proj")
	desktop := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(watched, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(desktop, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(desktop, "report.pdf")
	if err := os.WriteFile(outside, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := &reviewRoundCLIPlatform{}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	var stdout, stderr bytes.Buffer
	exitCode := Main([]string{
		"run", "--discard", "--watch-only", watched, "--",
		"/bin/sh", "-c", `rm "$1"`, "unring-review", outside,
	}, strings.NewReader(""), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("run exit = %d\nstdout: %s\nstderr: %s", exitCode, stdout.String(), stderr.String())
	}
	for _, literal := range []string{
		"CHANGE LIST IS NOT WHOLE-VOLUME",
		"limited by --watch-only",
		"Changes elsewhere are not reported",
		"even when the APFS snapshot contains them",
		"session can look clean",
	} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("watch-only disclosure omitted %q:\n%s", literal, stderr.String())
		}
	}
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	encoded := bytes.NewBuffer(nil)
	if exitCode := writeJSON(encoded, bytes.NewBuffer(nil), records[0].Files); exitCode != 0 {
		t.Fatalf("encode files summary exit = %d", exitCode)
	}
	if !strings.Contains(encoded.String(), `"change_list_scope"`) ||
		!strings.Contains(encoded.String(), `"watch-only"`) {
		t.Fatalf("audit omitted structured change-list limitation: %s", encoded.String())
	}

	wantDisclosure := fmt.Sprintf(""+
		"============================================================\n"+
		"CHANGE LIST IS NOT WHOLE-VOLUME\n"+
		"It is limited by --watch-only to: %s\n"+
		"Changes elsewhere are not reported or written to the audit record, even when the APFS snapshot contains them; the session can look clean after such a change.\n"+
		"============================================================", watched)
	liveDisclosure := changeListDisclosure(t, stderr.String())
	if liveDisclosure != wantDisclosure {
		t.Fatalf("live watch-only disclosure:\n%s\nwant:\n%s", liveDisclosure, wantDisclosure)
	}
	for _, command := range []string{"log", "restore"} {
		var storedStdout, storedStderr bytes.Buffer
		if exitCode := Main([]string{command, records[0].ID}, strings.NewReader(""), &storedStdout, &storedStderr); exitCode != 0 {
			t.Fatalf("%s stored session exit = %d; stderr: %s", command, exitCode, storedStderr.String())
		}
		storedDisclosure := changeListDisclosure(t, storedStdout.String())
		if storedDisclosure != liveDisclosure {
			t.Fatalf("%s disclosure drifted from live output:\n%s\nwant:\n%s", command, storedDisclosure, liveDisclosure)
		}
	}
}

func TestCLIDefaultScopeQualifiesWholeVolumeBackstopClaim(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	platform := &reviewRoundCLIPlatform{}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	var stdout, stderr bytes.Buffer
	if exitCode := Main([]string{"run", "--discard", "--", "/usr/bin/true"}, strings.NewReader(""), &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run exit = %d: %s", exitCode, stderr.String())
	}
	for _, literal := range []string{
		"CHANGE LIST IS NOT WHOLE-VOLUME",
		"Changes outside the home scan and clone roots are not reported",
		"/etc, /opt, /tmp, or another volume",
	} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("default disclosure omitted %q:\n%s", literal, stderr.String())
		}
	}
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	wantDisclosure := fmt.Sprintf(""+
		"============================================================\n"+
		"CHANGE LIST IS NOT WHOLE-VOLUME\n"+
		"Change reporting covers the home scan and clone roots: %s, %s\n"+
		"Changes outside the home scan and clone roots are not reported, including /etc, /opt, /tmp, or another volume, even when the APFS snapshot contains them.\n"+
		"============================================================", resolvedHome, project)
	liveDisclosure := changeListDisclosure(t, stderr.String())
	if liveDisclosure != wantDisclosure {
		t.Fatalf("live default-scope disclosure:\n%s\nwant:\n%s", liveDisclosure, wantDisclosure)
	}
	for _, command := range []string{"log", "restore"} {
		var storedStdout, storedStderr bytes.Buffer
		if exitCode := Main([]string{command, records[0].ID}, strings.NewReader(""), &storedStdout, &storedStderr); exitCode != 0 {
			t.Fatalf("%s stored session exit = %d; stderr: %s", command, exitCode, storedStderr.String())
		}
		if got := changeListDisclosure(t, storedStdout.String()); got != liveDisclosure {
			t.Fatalf("%s disclosure drifted from live output:\n%s\nwant:\n%s", command, got, liveDisclosure)
		}
	}
}

func TestStoredLegacyManifestMakesNoChangeListClaim(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	root := t.TempDir()
	platform := &reviewRoundCLIPlatform{}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	var runStdout, runStderr bytes.Buffer
	if exitCode := Main([]string{"run", "--discard", "--watch-only", root, "--", "/usr/bin/true"}, strings.NewReader(""), &runStdout, &runStderr); exitCode != 0 {
		t.Fatalf("run exit = %d: %s", exitCode, runStderr.String())
	}
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	manifestPath := filepath.Join(stateDir, "snapshots", records[0].ID, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "change_list_scope")
	delete(document, "change_list_roots")
	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"log", "restore"} {
		var stdout, stderr bytes.Buffer
		if exitCode := Main([]string{command, records[0].ID}, strings.NewReader(""), &stdout, &stderr); exitCode != 0 {
			t.Fatalf("%s legacy session exit = %d; stderr: %s", command, exitCode, stderr.String())
		}
		if strings.Contains(stdout.String(), "CHANGE LIST IS NOT WHOLE-VOLUME") {
			t.Fatalf("%s invented a scope claim for a legacy manifest:\n%s", command, stdout.String())
		}
	}
}

func changeListDisclosure(t *testing.T, output string) string {
	t.Helper()
	const boundary = "============================================================"
	var disclosure []string
	inside := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimPrefix(line, "unring: ")
		line = strings.TrimPrefix(line, "  ")
		if line == boundary {
			disclosure = append(disclosure, line)
			if inside {
				return strings.Join(disclosure, "\n")
			}
			inside = true
			continue
		}
		if inside {
			disclosure = append(disclosure, line)
		}
	}
	t.Fatalf("change-list disclosure not found in:\n%s", output)
	return ""
}

func TestCLIMountPermissionFailureNeverReportsRestored(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	cloneRoot := t.TempDir()
	outside := filepath.Join(home, "mount-denied.txt")
	if err := os.WriteFile(outside, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := &reviewRoundCLIPlatform{}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	var runStdout, runStderr bytes.Buffer
	if exitCode := Main([]string{
		"run", "--discard", "--watch", cloneRoot, "--",
		"/bin/sh", "-c", `rm "$1"`, "unring-review", outside,
	}, strings.NewReader(""), &runStdout, &runStderr); exitCode != 0 {
		t.Fatalf("run exit: %d\n%s", exitCode, runStderr.String())
	}
	store, err := audit.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, %v", len(records), err)
	}
	var changedPath string
	for _, change := range records[0].Files.Changes {
		if filepath.Base(change.Path) == filepath.Base(outside) {
			changedPath = change.Path
		}
	}
	if changedPath == "" {
		t.Fatalf("snapshot-only deletion absent: %#v", records[0].Files.Changes)
	}
	platform.mountErr = errors.New("mount_apfs: Operation not permitted")
	var stdout, stderr bytes.Buffer
	exitCode := Main([]string{"restore", records[0].ID, changedPath}, strings.NewReader(""), &stdout, &stderr)
	if exitCode != internalErrorExitCode {
		t.Fatalf("restore exit = %d, want %d", exitCode, internalErrorExitCode)
	}
	if strings.Contains(stdout.String(), "restored  ") {
		t.Fatalf("permission-denied mount reported restored: %s", stdout.String())
	}
	for _, literal := range []string{
		"ROOT PRIVILEGES REQUIRED",
		"mount volume snapshot",
		"Operation not permitted",
		"not restored",
	} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("mount failure omitted %q:\n%s", literal, stderr.String())
		}
	}
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("permission-denied restore recreated path: %v", err)
	}
}
