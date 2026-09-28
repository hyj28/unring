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

func TestRestoreRecordedBaselinesRecreatesNamedDirectorySubtree(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(directory, "x.txt")
	y := filepath.Join(nested, "y.txt")
	if err := os.WriteFile(x, []byte("literal interrupted x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(y, []byte("literal interrupted y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "interrupted-directory", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(20, 0), nil)
	if summary.Complete || !summary.Interrupted || len(summary.Changes) != 0 {
		t.Fatalf("interrupted directory fixture = %#v, want independently verified empty incomplete list", summary)
	}

	results, err := RestoreRecordedBaselines(stateDir, "interrupted-directory", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 4)
	assertRollbackTestFile(t, x, "literal interrupted x\n")
	assertRollbackTestFile(t, y, "literal interrupted y\n")
}

func TestRestoreRecordedBaselinesChecksEveryNamedDirectoryDescendant(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(directory, "x.txt")
	y := filepath.Join(nested, "y.txt")
	if err := os.WriteFile(x, []byte("literal baseline x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(y, []byte("literal baseline y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "interrupted-directory-conflict", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x, []byte("literal current x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(20, 0), nil)
	if summary.Complete || !summary.Interrupted || len(summary.Changes) != 0 {
		t.Fatalf("interrupted conflict fixture = %#v, want independently verified empty incomplete list", summary)
	}
	newPath := filepath.Join(directory, "new.txt")
	if err := os.WriteFile(newPath, []byte("literal later file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := RestoreRecordedBaselines(stateDir, "interrupted-directory-conflict", []string{directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRestorePaths(t, results, 5)
	byPath := restoreResultsByPath(results)
	if byPath[x].Status != "recorded-baseline-refused" || byPath[x].Sidecar == "" {
		t.Fatalf("conflicting baseline descendant = %#v, want refused with sidecar", byPath[x])
	}
	if byPath[y].Status != "recorded-baseline-already-present" {
		t.Fatalf("matching baseline descendant = %#v, want already present", byPath[y])
	}
	if byPath[newPath].Status != "recorded-baseline-unrecorded-present" {
		t.Fatalf("unrecorded current descendant = %#v, want left untouched", byPath[newPath])
	}
	assertRollbackTestFile(t, x, "literal current x\n")
	assertRollbackTestFile(t, byPath[x].Sidecar, "literal baseline x\n")
	assertRollbackTestFile(t, newPath, "literal later file\n")
}

func TestForcedRecordedBaselineDirectoryLeavesAndReportsUnrecordedDescendant(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	baseline := filepath.Join(directory, "baseline.txt")
	if err := os.WriteFile(baseline, []byte("literal forced baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "forced-directory-unrecorded", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(20, 0), nil)
	if summary.Complete || !summary.Interrupted || len(summary.Changes) != 0 {
		t.Fatalf("forced baseline fixture = %#v, want independent empty incomplete list", summary)
	}
	userFile := filepath.Join(directory, "later-user.txt")
	if err := os.WriteFile(userFile, []byte("literal later user bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := RestoreRecordedBaselines(stateDir, "forced-directory-unrecorded", []string{directory}, true)
	if err != nil {
		t.Fatal(err)
	}
	byPath := restoreResultsByPath(results)
	if byPath[userFile].Status != "recorded-baseline-unrecorded-present" {
		t.Fatalf("unrecorded descendant result = %#v, want left-untouched status", byPath[userFile])
	}
	assertRollbackTestFile(t, userFile, "literal later user bytes\n")
}

func TestForcedRecordedBaselineDirectoryReplacedByFileFailsTypeCheck(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "child.txt"), []byte("literal baseline child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, _, err := StartScope(stateDir, "forced-baseline-directory-file", Scope{Watched: []string{root}}, 1<<30, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("literal current replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary := session.SealContext(ctx, time.Unix(20, 0), nil)
	if summary.Complete || !summary.Interrupted || len(summary.Changes) != 0 {
		t.Fatalf("forced replacement fixture = %#v, want empty incomplete list", summary)
	}

	results, err := RestoreRecordedBaselines(stateDir, "forced-baseline-directory-file", []string{directory}, true)
	if err != nil {
		t.Fatal(err)
	}
	byPath := restoreResultsByPath(results)
	if byPath[directory].Status != "error" || byPath[directory].Err == nil ||
		!strings.Contains(byPath[directory].Err.Error(), "existing path has type") {
		t.Fatalf("forced baseline replacement result = %#v, want type error", byPath[directory])
	}
	assertRollbackTestFile(t, directory, "literal current replacement\n")
}
