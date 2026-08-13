package localrollback

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSealSuppressesByteIdenticalRewriteWithRestoredMTime(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "same.txt")
	writeRollbackTestFile(t, path, "identical bytes")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := Start(stateDir, "identical-rewrite", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("identical bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for _, change := range session.Seal(time.Now()).Changes {
		if change.Path == path {
			t.Fatalf("byte-identical rewrite reported as %#v", change)
		}
	}
}

func TestSealOverReportsLargeFileWhenAutomaticComparisonIsBounded(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	contents := bytes.Repeat([]byte{'x'}, 8*1024*1024+1)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := Start(stateDir, "large-fallback", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	assertRollbackChange(t, session.Seal(time.Now()).Changes, "modified", path)
}

func TestRestoreRecognizesOriginalBytesWithoutMatchingMTime(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeRollbackTestFile(t, path, "original")
	session, _, err := Start(stateDir, "manual-original", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "session change")
	session.Seal(time.Now())
	writeRollbackTestFile(t, path, "original")
	results, err := Restore(stateDir, "manual-original", []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "already-restored" || results[0].Sidecar != "" {
		t.Fatalf("restore results = %#v, want already-restored without sidecar", results)
	}
	if matches, err := filepath.Glob(path + ".unring-*.snapshot"); err != nil || len(matches) != 0 {
		t.Fatalf("conflict sidecars = %#v, %v, want none", matches, err)
	}
}

func TestRestoreStillRefusesGenuinelyDifferentBytes(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeRollbackTestFile(t, path, "original")
	session, _, err := Start(stateDir, "real-conflict", []string{root}, DefaultRetentionBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writeRollbackTestFile(t, path, "session change")
	session.Seal(time.Now())
	writeRollbackTestFile(t, path, "user change")
	results, err := Restore(stateDir, "real-conflict", []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "refused" || results[0].Sidecar == "" {
		t.Fatalf("restore results = %#v, want refused with sidecar", results)
	}
	assertRollbackTestFile(t, results[0].Sidecar, "original")
}

func TestPlanRetentionCombinesAgeAndCapAndKeepsNewest(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "snapshots")
	for _, value := range []manifest{
		{Version: manifestVersion, SessionID: "old", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), StorageBytes: 80, StorageExact: true},
		{Version: manifestVersion, SessionID: "new", StartedAt: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), StorageBytes: 80, StorageExact: true},
	} {
		directory := filepath.Join(root, value.SessionID)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeManifest(directory, value); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanRetention(stateDir, []StoredSession{
		{ID: "old", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "new", StartedAt: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)},
	}, 100, 14*24*time.Hour, time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removals) != 1 || plan.Removals[0].SessionID != "old" ||
		!plan.Removals[0].Expired || !plan.Removals[0].CapRequired {
		t.Fatalf("retention removals = %#v, want old selected once by both limits", plan.Removals)
	}
	if plan.After.Bytes != 80 || plan.After.Sessions != 1 {
		t.Fatalf("retention after = %#v, want literal 80 bytes and one newest session", plan.After)
	}
}

func TestRetentionDaysConfigDefaultsAndOverrides(t *testing.T) {
	stateDir := t.TempDir()
	days, err := RetentionDaysForState(stateDir)
	if err != nil || days != 14 {
		t.Fatalf("default retention days = %d, %v, want 14", days, err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	days, err = RetentionDaysForState(stateDir)
	if err != nil || days != 30 {
		t.Fatalf("configured retention days = %d, %v, want 30", days, err)
	}
}
