package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

func TestAutomaticRetentionStopsOnHardAuditListFailure(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	runMainSession(t, t.TempDir(), "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	restoreLogs := replaceLogsDirectoryWithFile(t, stateDir)
	defer restoreLogs()
	var output strings.Builder
	summary := localrollback.Summary{}
	applyAutomaticRetention(store, "", 0, 1, &summary, &output)
	if !strings.Contains(output.String(), "cannot inspect stored sessions") {
		t.Fatalf("hard audit failure was not reported:\n%s", output.String())
	}
	if got := snapshotStoreCount(t, stateDir); got != 1 {
		t.Fatalf("snapshot count after hard audit failure = %d, want literal 1", got)
	}
	if len(summary.RetentionEvents) != 0 {
		t.Fatalf("retention events = %#v, want none", summary.RetentionEvents)
	}
}

func TestPruneConfirmationFailsClosedOnAuditListFailure(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	runMainSession(t, t.TempDir(), "/usr/bin/true")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	preview := prunePreview{
		Version: prunePreviewVersion, Created: time.Now(),
		Removals: []localrollback.RetentionRemoval{{SessionID: "candidate", Expired: true}},
	}
	restoreLogs := replaceLogsDirectoryWithFile(t, stateDir)
	defer restoreLogs()
	err = validatePrunePreview(store, preview, time.Now())
	if err == nil || !strings.Contains(err.Error(), "cannot verify the current newest session") {
		t.Fatalf("validation error = %v, want fail-closed audit error", err)
	}
	if got := snapshotStoreCount(t, stateDir); got != 1 {
		t.Fatalf("snapshot count after failed validation = %d, want literal 1", got)
	}
}

func TestPrunePreviewExpiresAndLaterPruneCollectsIt(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	expiredToken, err := savePrunePreview(stateDir, []localrollback.RetentionRemoval{{SessionID: "expired", Expired: true}})
	if err != nil {
		t.Fatal(err)
	}
	currentToken, err := savePrunePreview(stateDir, []localrollback.RetentionRemoval{{SessionID: "current", Expired: true}})
	if err != nil {
		t.Fatal(err)
	}
	path := prunePreviewPath(stateDir, expiredToken)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var preview prunePreview
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatal(err)
	}
	preview.Created = time.Now().Add(-25 * time.Hour)
	data, err = json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPrunePreview(stateDir, expiredToken, time.Now()); err == nil || !strings.Contains(err.Error(), "expired after 24 hours") {
		t.Fatalf("expired preview load error = %v", err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("cleanup prune exit = %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired preview remains: %v", err)
	}
	if _, err := os.Stat(prunePreviewPath(stateDir, currentToken)); err != nil {
		t.Fatalf("current preview was collected: %v", err)
	}
}

func TestPruneConfirmationRefusesTargetAfterCapIsRaised(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	for range 2 {
		runMainSession(t, t.TempDir(), "/usr/bin/true")
	}
	if err := localrollback.SaveRetentionCap(stateDir, 1); err != nil {
		t.Fatal(err)
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var previewOut, previewErr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &previewOut, &previewErr); code != 0 {
		t.Fatalf("preview exit = %d: %s", code, previewErr.String())
	}
	token := prunePreviewToken(t, previewOut.String())
	if err := localrollback.SaveRetentionCap(stateDir, 1<<40); err != nil {
		t.Fatal(err)
	}
	var confirmOut, confirmErr strings.Builder
	if code := Main([]string{"prune", "--confirm", token}, strings.NewReader(""), &confirmOut, &confirmErr); code != internalErrorExitCode {
		t.Fatalf("stale cap confirmation exit = %d, want %d", code, internalErrorExitCode)
	}
	if !strings.Contains(confirmErr.String(), "no longer in the same retention set") {
		t.Fatalf("stale cap reason missing:\n%s", confirmErr.String())
	}
	if strings.Count(confirmErr.String(), "run unring prune again") != 1 {
		t.Fatalf("stale cap remedy was not printed exactly once:\n%s", confirmErr.String())
	}
	if got := storedRecordCount(t, store); got != 2 {
		t.Fatalf("records after stale cap confirmation = %d, want literal 2", got)
	}
	if got := snapshotStoreCount(t, stateDir); got != 2 {
		t.Fatalf("snapshots after stale cap confirmation = %d, want literal 2", got)
	}
}

func TestPruneConfirmationRefusesTargetAfterRetentionAgeIsRaised(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	for range 2 {
		runMainSession(t, t.TempDir(), "/usr/bin/true")
	}
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
	var previewOut, previewErr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &previewOut, &previewErr); code != 0 {
		t.Fatalf("preview exit = %d: %s", code, previewErr.String())
	}
	token := prunePreviewToken(t, previewOut.String())
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var confirmOut, confirmErr strings.Builder
	if code := Main([]string{"prune", "--confirm", token}, strings.NewReader(""), &confirmOut, &confirmErr); code != internalErrorExitCode {
		t.Fatalf("stale age confirmation exit = %d, want %d", code, internalErrorExitCode)
	}
	if !strings.Contains(confirmErr.String(), "no longer in the same retention set") {
		t.Fatalf("stale age reason missing:\n%s", confirmErr.String())
	}
	if got := storedRecordCount(t, store); got != 2 {
		t.Fatalf("records after stale age confirmation = %d, want literal 2", got)
	}
	if got := snapshotStoreCount(t, stateDir); got != 2 {
		t.Fatalf("snapshots after stale age confirmation = %d, want literal 2", got)
	}
}

func TestExpiredPruneConfirmationCollectsTokenWithoutRemovingSession(t *testing.T) {
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
	token, err := savePrunePreview(stateDir, []localrollback.RetentionRemoval{{
		SessionID: records[0].ID, HasSnapshot: true, Expired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := prunePreviewPath(stateDir, token)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var preview prunePreview
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatal(err)
	}
	preview.Created = time.Now().Add(-25 * time.Hour)
	data, err = json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune", "--confirm", token}, strings.NewReader(""), &stdout, &stderr); code != usageExitCode {
		t.Fatalf("expired confirmation exit = %d, want %d", code, usageExitCode)
	}
	if !strings.Contains(stderr.String(), "expired after 24 hours") {
		t.Fatalf("expiry reason missing:\n%s", stderr.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired confirmation token remains: %v", err)
	}
	if storedRecordCount(t, store) != 1 || snapshotStoreCount(t, stateDir) != 1 {
		t.Fatalf("expired confirmation removed its target")
	}
}

func TestPruneCollectsOldInvalidPreviewFile(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	directory := filepath.Join(stateDir, "prune-previews")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "broken.json")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"prune"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("cleanup prune exit = %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old invalid preview remains: %v", err)
	}
}

func TestSnapshotsReportsKnownUsageAlongsideDamagedStore(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	runMainSession(t, t.TempDir(), "/usr/bin/true")
	if err := os.MkdirAll(filepath.Join(stateDir, "snapshots", "damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"snapshots"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("snapshots exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "known bytes") || !strings.Contains(stdout.String(), "2 sessions retained; usage is incomplete") {
		t.Fatalf("incomplete usage output missing:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "snapshot storage warning for damaged") {
		t.Fatalf("damaged usage warning missing:\n%s", stderr.String())
	}
}

func replaceLogsDirectoryWithFile(t *testing.T, stateDir string) func() {
	t.Helper()
	logs := filepath.Join(stateDir, "logs")
	saved := filepath.Join(stateDir, "logs.saved-for-test")
	if err := os.Rename(logs, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logs, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(logs); err != nil {
			t.Error(err)
		}
		if err := os.Rename(saved, logs); err != nil {
			t.Error(err)
		}
	}
}
