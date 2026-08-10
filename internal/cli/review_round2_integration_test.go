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

func TestCLIReportsUncapturableDeletionForExplicitWatchScopes(t *testing.T) {
	for _, test := range []struct {
		name string
		flag string
	}{
		{name: "watch-only inside home", flag: "--watch-only"},
		{name: "additive watch outside home", flag: "--watch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
			t.Setenv("DATABASE_URL", "")
			t.Setenv("UNRING_STATE_DIR", t.TempDir())
			base := t.TempDir()
			home := filepath.Join(base, "home")
			project := filepath.Join(home, "project")
			watched := filepath.Join(home, "watched")
			if test.flag == "--watch" {
				watched = filepath.Join(base, "outside")
			}
			for _, path := range []string{project, watched} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Chdir(project)
			deleted := filepath.Join(watched, "a.txt")
			twin := filepath.Join(watched, "a-twin.txt")
			if err := os.WriteFile(deleted, []byte("hard-linked before"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(deleted, twin); err != nil {
				t.Fatal(err)
			}
			platform := &reviewRoundCLIPlatform{}
			restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
			defer restorePlatform()
			var stdout, stderr bytes.Buffer
			exitCode := Main([]string{
				"run", "--discard", test.flag, watched, "--",
				"/bin/sh", "-c", `rm "$1"`, "unring-review", deleted,
			}, strings.NewReader(""), &stdout, &stderr)
			if exitCode != 0 {
				t.Fatalf("run exit = %d\nstdout: %s\nstderr: %s", exitCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "deleted  "+deleted) {
				t.Fatalf("uncapturable deletion not reported:\n%s", stdout.String())
			}
			store, err := audit.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			records, err := store.List()
			if err != nil || len(records) != 1 {
				t.Fatalf("records = %d, %v", len(records), err)
			}
			var change *localrollback.Change
			for index := range records[0].Files.Changes {
				if records[0].Files.Changes[index].Path == deleted {
					change = &records[0].Files.Changes[index]
				}
			}
			if change == nil || change.Kind != "deleted" {
				t.Fatalf("audit change = %#v, want deleted path", change)
			}
		})
	}
}

func TestCLIFailedHomeScanClaimsCloneRootsOnly(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	missingHome := filepath.Join(t.TempDir(), "missing-home")
	t.Setenv("HOME", missingHome)
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
		"WIDENED CHANGE LIST UNAVAILABLE",
		"Change reporting is limited to the clone roots",
		"CHANGE-LIST SCAN INCOMPLETE: home directory:",
	} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("failed-scan disclosure omitted %q:\n%s", literal, stderr.String())
		}
	}
	for _, lie := range []string{"covers the home scan", "CHANGE-LIST SCAN INCOMPLETE: :"} {
		if strings.Contains(stderr.String(), lie) {
			t.Fatalf("failed-scan disclosure contains %q:\n%s", lie, stderr.String())
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
	if records[0].Files.ChangeListScope != localrollback.ChangeListScopeCloneOnly {
		t.Fatalf("change-list scope = %q, want clone-only", records[0].Files.ChangeListScope)
	}
	if len(records[0].Files.ScanFailures) != 1 || records[0].Files.ScanFailures[0].Path == "" {
		t.Fatalf("scan failures = %#v, want a nonempty path label", records[0].Files.ScanFailures)
	}
	if !errors.Is(os.Remove(missingHome), os.ErrNotExist) {
		t.Fatal("missing home unexpectedly exists")
	}
}
