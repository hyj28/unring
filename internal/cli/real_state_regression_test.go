package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

func TestRestoreListingMarksMissingClonePathsUnavailable(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	clonePath := filepath.Join(t.TempDir(), "clone-only.txt")
	volumePath := filepath.Join(t.TempDir(), "volume-only.txt")
	record := newRealStateRecord(t, time.Now(), "evicted")
	record.Files.Retained = true // Legacy records can still claim this after the store disappeared.
	record.Files.Changes = []localrollback.Change{
		{Kind: "modified", Path: clonePath, RestoreSource: localrollback.RestoreSourceClone},
		{
			Kind: "deleted", Path: volumePath, RestoreSource: localrollback.RestoreSourceVolume,
			VolumeSnapshot: &localrollback.VolumeSnapshot{
				Name: "com.apple.TimeMachine.2026-08-09-130000.local", VolumeID: "disk-test", MountPoint: "/",
			},
		},
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("restore listing exit = %d: %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		clonePath,
		"NOT RESTORABLE: clone snapshot data was evicted; this path was not stored in the volume-snapshot restore layer",
		volumePath,
		"SNAPSHOT ONLY: restore requires sudo",
		"Clone snapshot data has been evicted; SNAPSHOT ONLY paths remain restorable",
		"Restore selected snapshot-only paths with: unring restore " + record.ID,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("restore listing missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Restore every restorable path") {
		t.Fatalf("evicted clone listing offered an unusable --all command:\n%s", output)
	}
}

func TestRestoreListingWithOnlyMissingCloneOffersNoRestoreCommand(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	record := newRealStateRecord(t, time.Now(), "evicted-only")
	record.Files.Retained = true
	record.Files.Changes = []localrollback.Change{{
		Kind: "deleted", Path: filepath.Join(t.TempDir(), "gone.txt"),
		RestoreSource: localrollback.RestoreSourceClone,
	}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("restore listing exit = %d: %s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "NOT RESTORABLE: clone snapshot data was evicted") ||
		!strings.Contains(output, "these changes are no longer restorable") {
		t.Fatalf("evicted-only listing did not disclose lost restore data:\n%s", output)
	}
	if strings.Contains(output, "Restore selected") || strings.Contains(output, "Restore every") || strings.Contains(output, "Include this group") {
		t.Fatalf("evicted-only listing offered a restore command:\n%s", output)
	}
}

func TestSnapshotsDistinguishesCloneDataFromAuditOnlySession(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	runMainSession(t, t.TempDir(), "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want one", records, err)
	}
	retainedID := records[0].ID
	auditOnly := newRealStateRecord(t, time.Now().Add(-time.Hour), "audit-only")
	auditOnly.Files.Retained = true
	if err := store.Save(auditOnly); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"snapshots"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("snapshots exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), retainedID+"  CLONE STORE RETAINED") {
		t.Fatalf("retained clone status missing:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), auditOnly.ID) || !strings.Contains(stdout.String(), "1 audit-only sessions with no currently restorable file snapshot data were omitted") {
		t.Fatalf("default snapshots did not summarize audit-only session:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"snapshots", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("snapshots --all exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), auditOnly.ID+"  NO RESTORABLE FILE SNAPSHOT DATA — clone store absent") {
		t.Fatalf("snapshots --all did not distinguish audit-only session:\n%s", stdout.String())
	}
}

func TestSnapshotsBoundsDefaultAndAllShowsEverySession(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	platform := &cliBackstopPlatform{created: true, present: true, excluded: map[string]bool{}}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	for index := range 52 {
		record := newRealStateRecord(t, time.Now().Add(-time.Duration(index)*time.Second), "volume")
		record.Files.Backstop = localrollback.Backstop{
			Checked: true, Available: true,
			Snapshots: []localrollback.VolumeSnapshot{{
				Name: "com.apple.TimeMachine.2026-08-09-130000.local", VolumeID: "disk-cli-apfs", MountPoint: "/",
			}},
		}
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"snapshots"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("bounded snapshots exit = %d: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "VOLUME SNAPSHOT:"); got != 50 {
		t.Fatalf("bounded snapshot rows = %d, want literal 50:\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Showing the newest 50 of 52 sessions") || !strings.Contains(stdout.String(), "unring snapshots --all") {
		t.Fatalf("snapshot truncation disclosure missing:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"snapshots", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("snapshots --all exit = %d: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "VOLUME SNAPSHOT:"); got != 52 {
		t.Fatalf("all snapshot rows = %d, want literal 52", got)
	}
}

func TestPruneBoundsDefaultButAllNamesCompleteSetBeforeToken(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for index := range 52 {
		record := newRealStateRecord(t, now.Add(-time.Duration(index+3)*24*time.Hour), "expired")
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	newest := newRealStateRecord(t, now, "newest")
	if err := store.Save(newest); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("bounded prune exit = %d: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "would remove session "); got != 50 {
		t.Fatalf("bounded prune rows = %d, want literal 50:\n%s", got, stdout.String())
	}
	for _, want := range []string{
		"Showing 50 of 52 sessions in the retention set",
		"use unring prune --all",
		"no confirmation token was issued",
		"this removes only the audit record; no snapshot data remains",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("bounded prune missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "unring prune --confirm") {
		t.Fatalf("truncated prune issued a destructive confirmation command:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"prune", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("prune --all exit = %d: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "would remove session "); got != 52 {
		t.Fatalf("all prune rows = %d, want literal 52", got)
	}
	if !strings.Contains(stdout.String(), "unring prune --confirm ") {
		t.Fatalf("complete prune listing omitted confirmation token:\n%s", stdout.String())
	}
}

func TestSnapshotsAuditOnlyBackstopStatusMatrix(t *testing.T) {
	tests := []struct {
		name       string
		backstop   localrollback.Backstop
		platform   localrollback.VolumeSnapshotPlatform
		want       string
		wantAbsent string
	}{
		{
			name: "never recorded",
			want: "NO RESTORABLE FILE SNAPSHOT DATA — clone store absent; no whole-volume backstop: no backstop was recorded",
		},
		{
			name:     "unavailable",
			backstop: localrollback.Backstop{Checked: true, Reason: "Time Machine is disabled"},
			want:     "NO RESTORABLE FILE SNAPSHOT DATA — clone store absent; no whole-volume backstop: Time Machine is disabled",
		},
		{
			name:     "purged",
			backstop: realStateBackstop(),
			platform: &cliBackstopPlatform{created: true, present: false, excluded: map[string]bool{}},
			want:     "NO RESTORABLE FILE SNAPSHOT DATA — clone store absent; com.apple.TimeMachine.2026-08-09-130000.local on / — PURGED OR DELETED",
		},
		{
			name:       "present",
			backstop:   realStateBackstop(),
			platform:   &cliBackstopPlatform{created: true, present: true, excluded: map[string]bool{}},
			want:       "NO CLONE STORE; VOLUME SNAPSHOT: com.apple.TimeMachine.2026-08-09-130000.local on / — present",
			wantAbsent: "NO RESTORABLE FILE SNAPSHOT DATA",
		},
		{
			name:       "presence unknown",
			backstop:   realStateBackstop(),
			platform:   &snapshotListErrorPlatform{cliBackstopPlatform: cliBackstopPlatform{created: true, excluded: map[string]bool{}}},
			want:       "NO CLONE STORE; VOLUME SNAPSHOT: com.apple.TimeMachine.2026-08-09-130000.local on / — presence unknown: list denied",
			wantAbsent: "NO RESTORABLE FILE SNAPSHOT DATA",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			configureStorageHygieneTest(t, stateDir)
			store, err := audit.OpenStoreAt(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			record := newRealStateRecord(t, time.Now(), test.name)
			record.Files.Backstop = test.backstop
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			if test.platform != nil {
				restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(test.platform)
				defer restorePlatform()
			}
			var stdout, stderr strings.Builder
			if code := Main([]string{"snapshots", "--all"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("snapshots --all exit = %d: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("snapshot status missing %q:\n%s", test.want, stdout.String())
			}
			if test.wantAbsent != "" && strings.Contains(stdout.String(), test.wantAbsent) {
				t.Fatalf("snapshot status unexpectedly contained %q:\n%s", test.wantAbsent, stdout.String())
			}
		})
	}
}

func TestBoundedSessionCommandsRejectAmbiguousArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "snapshots positional argument", args: []string{"snapshots", "extra"}, want: "Usage: unring snapshots [--all]"},
		{name: "prune positional argument", args: []string{"prune", "extra"}, want: "Usage: unring prune [--all] [--confirm preview-token]"},
		{name: "prune all with confirmation", args: []string{"prune", "--all", "--confirm", "000000000000000000000000"}, want: "--all cannot be combined with --confirm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configureStorageHygieneTest(t, t.TempDir())
			var stdout, stderr strings.Builder
			if code := Main(test.args, strings.NewReader(""), &stdout, &stderr); code != usageExitCode {
				t.Fatalf("exit = %d, want %d", code, usageExitCode)
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr missing %q:\n%s", test.want, stderr.String())
			}
		})
	}
}

func realStateBackstop() localrollback.Backstop {
	return localrollback.Backstop{
		Checked: true, Available: true,
		Snapshots: []localrollback.VolumeSnapshot{{
			Name: "com.apple.TimeMachine.2026-08-09-130000.local", VolumeID: "disk-cli-apfs", MountPoint: "/",
		}},
	}
}

type snapshotListErrorPlatform struct {
	cliBackstopPlatform
}

func (*snapshotListErrorPlatform) ListSnapshots(localrollback.Volume) ([]string, error) {
	return nil, errors.New("list denied")
}

func newRealStateRecord(t *testing.T, started time.Time, command string) audit.Record {
	t.Helper()
	record, err := audit.NewRecord([]string{command}, started)
	if err != nil {
		t.Fatal(err)
	}
	record.Outcome = "discarded"
	return record
}
