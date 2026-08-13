package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
)

func TestPrunePreviewsThenConfirmsTheSameSessions(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	watched := t.TempDir()
	runMainSession(t, watched, "/usr/bin/true")
	runMainSession(t, watched, "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %#v, %v, want two", records, err)
	}
	oldID := records[1].ID
	records[1].StartedAt = time.Now().Add(-20 * 24 * time.Hour)
	if err := store.Save(records[1]); err != nil {
		t.Fatal(err)
	}

	var previewOut, previewErr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &previewOut, &previewErr); code != 0 {
		t.Fatalf("prune preview exit = %d\nstdout:\n%s\nstderr:\n%s", code, previewOut.String(), previewErr.String())
	}
	for _, want := range []string{"would remove session " + oldID, "measured retained-snapshot bytes/references", "No sessions were removed", "copy-on-write clones"} {
		if !strings.Contains(previewOut.String(), want) {
			t.Fatalf("prune preview missing %q:\n%s", want, previewOut.String())
		}
	}
	if got := storedRecordCount(t, store); got != 2 {
		t.Fatalf("record count after preview = %d, want 2", got)
	}
	if got := snapshotStoreCount(t, stateDir); got != 2 {
		t.Fatalf("snapshot count after preview = %d, want 2", got)
	}

	var confirmOut, confirmErr strings.Builder
	if code := Main([]string{"prune", "--confirm"}, strings.NewReader(""), &confirmOut, &confirmErr); code != 0 {
		t.Fatalf("confirmed prune exit = %d\nstdout:\n%s\nstderr:\n%s", code, confirmOut.String(), confirmErr.String())
	}
	if !strings.Contains(confirmOut.String(), "removed session "+oldID) {
		t.Fatalf("confirmed prune did not name removal:\n%s", confirmOut.String())
	}
	if got := storedRecordCount(t, store); got != 1 {
		t.Fatalf("record count after confirmed prune = %d, want 1", got)
	}
	if got := snapshotStoreCount(t, stateDir); got != 1 {
		t.Fatalf("snapshot count after confirmed prune = %d, want 1", got)
	}
}

func TestPruneKeepsTheNewestSessionEvenWhenExpired(t *testing.T) {
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
	records[0].StartedAt = time.Now().Add(-40 * 24 * time.Hour)
	if err := store.Save(records[0]); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune", "--confirm"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("prune exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing to prune") || storedRecordCount(t, store) != 1 {
		t.Fatalf("newest expired session was not protected:\n%s", stdout.String())
	}
}

func TestRunAutomaticallyExpiresOldSessionAndRecordsAnnouncement(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	watched := t.TempDir()
	runMainSession(t, watched, "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want one", records, err)
	}
	oldID := records[0].ID
	records[0].StartedAt = time.Now().Add(-2 * 24 * time.Hour)
	if err := store.Save(records[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runMainSession(t, watched, "/usr/bin/true")
	combined := stdout + stderr
	for _, want := range []string{"retention expired session " + oldID, "past the configured age", "measured retained-snapshot bytes/references"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("automatic expiry output missing %q:\n%s", want, combined)
		}
	}
	if _, err := store.Load(oldID); err == nil {
		t.Fatalf("expired audit record %s still exists", oldID)
	}
	records, err = store.List()
	if err != nil || len(records) != 1 || len(records[0].Files.RetentionEvents) != 1 || records[0].Files.RetentionEvents[0].SessionID != oldID {
		t.Fatalf("current record did not retain expiry announcement: records=%#v err=%v", records, err)
	}
}

func TestConfiguredThirtyDayRetentionDoesNotUseFourteenDayDefault(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	watched := t.TempDir()
	runMainSession(t, watched, "/usr/bin/true")
	runMainSession(t, watched, "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %#v, %v, want two", records, err)
	}
	records[1].StartedAt = time.Now().Add(-20 * 24 * time.Hour)
	if err := store.Save(records[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("prune exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing to prune") || storedRecordCount(t, store) != 2 {
		t.Fatalf("30-day configuration did not preserve 20-day session:\n%s", stdout.String())
	}
}

func TestLogDefaultIsLiterallyFiftyAndAllShowsEverything(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 52; index++ {
		record, err := audit.NewRecord([]string{fmt.Sprintf("cmd-%02d", index)}, time.Date(2026, 1, 1, 0, index, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	var boundedOut, boundedErr strings.Builder
	if code := Main([]string{"log"}, strings.NewReader(""), &boundedOut, &boundedErr); code != 0 {
		t.Fatalf("bounded log exit = %d: %s", code, boundedErr.String())
	}
	if got := strings.Count(boundedOut.String(), "cmd-"); got != 50 {
		t.Fatalf("bounded log command count = %d, want literal 50\n%s", got, boundedOut.String())
	}
	if !strings.Contains(boundedOut.String(), "Showing the newest 50 of 52 sessions") || !strings.Contains(boundedOut.String(), "unring log --all") {
		t.Fatalf("bounded log omitted truncation disclosure:\n%s", boundedOut.String())
	}
	var allOut, allErr strings.Builder
	if code := Main([]string{"log", "--all"}, strings.NewReader(""), &allOut, &allErr); code != 0 {
		t.Fatalf("all log exit = %d: %s", code, allErr.String())
	}
	if got := strings.Count(allOut.String(), "cmd-"); got != 52 {
		t.Fatalf("all log command count = %d, want literal 52", got)
	}
}

func TestRunEscapesNewlinePathButStoresRealPath(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	watched := t.TempDir()
	path := filepath.Join(watched, "line\nbreak.txt")
	stdout, stderr := runMainSession(t, watched, "/bin/sh", "-c", `printf x > "$1"`, "sh", path)
	combined := stdout + stderr
	if !strings.Contains(combined, strconv.Quote(path)) || strings.Contains(combined, "line\nbreak.txt") {
		t.Fatalf("newline path was not rendered on one escaped line:\n%s", combined)
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || len(records[0].Files.Changes) != 1 || records[0].Files.Changes[0].Path != path {
		t.Fatalf("stored path changed: records=%#v err=%v", records, err)
	}
	var logOut, logErr strings.Builder
	if code := Main([]string{"log", records[0].ID}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("stored rendering exit = %d: %s", code, logErr.String())
	}
	if !strings.Contains(logOut.String(), strconv.Quote(path)) || strings.Contains(logOut.String(), "line\nbreak.txt") {
		t.Fatalf("stored rendering disagreed with live escaped path:\n%s", logOut.String())
	}
}

func TestRestoreCommandRecognizesManuallyRestoredBytes(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	watched := t.TempDir()
	path := filepath.Join(watched, "file.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMainSession(t, watched, "/bin/sh", "-c", `printf 'session change' > "$1"`, "sh", path)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want one", records, err)
	}
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeStores := snapshotStoreCount(t, stateDir)
	beforeRecords := storedRecordCount(t, store)
	var stdout, stderr strings.Builder
	if code := Main([]string{"restore", records[0].ID, path}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "already restored  "+path) {
		t.Fatalf("restore did not report already restored:\n%s", stdout.String())
	}
	if matches, err := filepath.Glob(path + ".unring-*.snapshot"); err != nil || len(matches) != 0 {
		t.Fatalf("sidecars = %#v, %v, want none", matches, err)
	}
	if got := snapshotStoreCount(t, stateDir); got != beforeStores {
		t.Fatalf("restore changed snapshot store count from %d to %d", beforeStores, got)
	}
	if got := storedRecordCount(t, store); got != beforeRecords {
		t.Fatalf("restore changed audit record count from %d to %d", beforeRecords, got)
	}
}

func configureStorageHygieneTest(t *testing.T, stateDir string) {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("UNRING_SNAPSHOT_CAP_BYTES", strconv.FormatInt(1<<40, 10))
}

func runMainSession(t *testing.T, watched string, command ...string) (string, string) {
	t.Helper()
	args := []string{"run", "--snapshot-cap-bytes", strconv.FormatInt(1<<40, 10), "--watch-only", watched, "--"}
	args = append(args, command...)
	var stdout, stderr strings.Builder
	if code := Main(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func storedRecordCount(t *testing.T, store *audit.Store) int {
	t.Helper()
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func snapshotStoreCount(t *testing.T, stateDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "snapshots"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			count++
		}
	}
	return count
}
