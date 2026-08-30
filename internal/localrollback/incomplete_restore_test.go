package localrollback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreRecordedBaselinesReachesInterruptedCloneWithoutClaimingAChange(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(root, "reports", "2024-summary.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("recorded baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "interrupted-baseline-session", Scope{
		Watched: []string{root},
	}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("contents after start\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(root, "leaked-secrets.env")
	if err := os.WriteFile(created, []byte("session-created contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(20, 0), nil)
	if summary.Complete || !summary.Interrupted || len(summary.Changes) != 0 {
		t.Fatalf("interrupted summary = %#v, want incomplete with an independently expected empty change list", summary)
	}
	if _, err := ChangesForRestore(summary.Changes, []string{path}); err == nil {
		t.Fatal("change-list restore selection unexpectedly reached a path absent from the change list")
	}

	results, err := RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{path}, false)
	if err != nil {
		t.Fatalf("RestoreRecordedBaselines() error: %v", err)
	}
	if len(results) != 1 || results[0].Path != path || results[0].Status != "recorded-baseline-refused" ||
		results[0].Err != nil || results[0].Sidecar == "" {
		t.Fatalf("protected baseline restore results = %#v, want one literal refused result with sidecar", results)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "contents after start\n" {
		t.Fatalf("refused baseline restore contents = %q, %v; want independent current literal", contents, err)
	}
	sidecarContents, err := os.ReadFile(results[0].Sidecar)
	if err != nil || string(sidecarContents) != "recorded baseline\n" {
		t.Fatalf("baseline sidecar contents = %q, %v; want independent baseline literal", sidecarContents, err)
	}
	results, err = RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{path}, true)
	if err != nil || len(results) != 1 || results[0].Status != "recorded-baseline-restored" {
		t.Fatalf("forced baseline restore results = %#v, %v, want literal restored status", results, err)
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "recorded baseline\n" {
		t.Fatalf("forced baseline contents = %q, %v; want independent baseline literal", contents, err)
	}

	outside := filepath.Join(t.TempDir(), "not-covered.txt")
	results, err = RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{outside}, false)
	if err != nil {
		t.Fatalf("outside baseline returned command error: %v", err)
	}
	if len(results) != 1 || results[0].Path != outside || results[0].Status != "unavailable" ||
		results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "outside the recorded snapshot roots") {
		t.Fatalf("outside baseline results = %#v, want one named unavailable result", results)
	}

	results, err = RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{created}, false)
	if err != nil || len(results) != 1 || results[0].Status != "recorded-absence-refused" {
		t.Fatalf("default recorded-absence results = %#v, %v, want literal refused status", results, err)
	}
	if contents, err := os.ReadFile(created); err != nil || string(contents) != "session-created contents\n" {
		t.Fatalf("default recorded-absence path = %q, %v; want independent retained literal", contents, err)
	}
	results, err = RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{created}, true)
	if err != nil || len(results) != 1 || results[0].Status != "recorded-absence-removed" {
		t.Fatalf("forced recorded-absence results = %#v, %v, want literal removed status", results, err)
	}
	if _, err := os.Lstat(created); !os.IsNotExist(err) {
		t.Fatalf("forced recorded-absence path still exists: %v", err)
	}

	if err := os.WriteFile(path, []byte("changed before mixed batch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err = RestoreRecordedBaselines(stateDir, "interrupted-baseline-session", []string{outside, path}, true)
	if err != nil {
		t.Fatalf("mixed RestoreRecordedBaselines() error: %v", err)
	}
	if len(results) != 2 || results[0].Path != outside || results[0].Status != "unavailable" ||
		results[1].Path != path || results[1].Status != "recorded-baseline-restored" {
		t.Fatalf("mixed baseline results = %#v, want independent unavailable then restored literals", results)
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "recorded baseline\n" {
		t.Fatalf("mixed baseline restored contents = %q, %v; want independent literal", contents, err)
	}
}
