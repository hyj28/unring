package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

func TestRestoreCommandNamedDirectoryReportsAndRestoresEveryRecordedPathOnce(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(directory, "x.txt")
	y := filepath.Join(nested, "y.txt")
	writeTestFile(t, x, "literal cli x\n")
	writeTestFile(t, y, "literal cli y\n")
	record := captureCLIRecord(t, stateDir, root, "literal-directory-command", func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
	}, false)
	if len(record.Files.Changes) != 4 {
		t.Fatalf("fixture changes = %#v, want independent four-path deletion", record.Files.Changes)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, x, "literal later x\n")

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory, x}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("directory conflict exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	for _, line := range []string{
		"already restored  " + directory + "\n",
		"restored  " + nested + "\n",
		"restored  " + y + "\n",
		"refused   " + x + ": changed after the session ended; not overwritten\n",
	} {
		if !strings.Contains(stdout.String()+stderr.String(), line) {
			t.Fatalf("restore output omitted whole line %q\nstdout:\n%s\nstderr:\n%s", line, stdout.String(), stderr.String())
		}
	}
	for _, path := range []string{nested, y} {
		if strings.Contains(stdout.String(), "already restored  "+path+"\n") {
			t.Fatalf("restored descendant was also reported already restored for %s:\n%s", path, stdout.String())
		}
	}
	assertTestFile(t, x, "literal later x\n")
	assertTestFile(t, y, "literal cli y\n")
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.LoadExact(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueCLIRestoreEvents(t, stored.Files.RestoreEvents, 4)

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"restore", "--force", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("forced directory restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	assertTestFile(t, x, "literal cli x\n")
	assertTestFile(t, y, "literal cli y\n")
}

func TestRestoreCommandInterruptedNamedDirectoryUsesRecordedSubtree(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(directory, "x.txt")
	y := filepath.Join(nested, "y.txt")
	writeTestFile(t, x, "literal interrupted cli x\n")
	writeTestFile(t, y, "literal interrupted cli y\n")
	record := captureCLIRecord(t, stateDir, root, "literal-interrupted-directory-command", func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
	}, true)
	if record.Files.Complete || !record.Files.Interrupted || len(record.Files.Changes) != 0 {
		t.Fatalf("interrupted CLI fixture = %#v, want independent empty incomplete list", record.Files)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("interrupted directory restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "change list incomplete") {
		t.Fatalf("interrupted restore output omitted incomplete disclosure:\n%s", stdout.String())
	}
	for _, path := range []string{directory, nested, x, y} {
		line := "recorded baseline written back  " + path + " — change list incomplete; unring cannot confirm what the session did to this path\n"
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("interrupted restore output omitted whole line %q:\n%s", line, stdout.String())
		}
	}
	assertTestFile(t, x, "literal interrupted cli x\n")
	assertTestFile(t, y, "literal interrupted cli y\n")
}

func TestRestoreCommandFreshDirectoryReportsRestoredWholeLines(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	nested := filepath.Join(directory, "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "x.txt"), "literal fresh CLI x\n")
	writeTestFile(t, filepath.Join(nested, "y.txt"), "literal fresh CLI y\n")
	record := captureCLIRecord(t, stateDir, root, "literal-fresh-directory", func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
	}, false)

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("fresh directory restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, path := range []string{directory, nested} {
		if !strings.Contains(stdout.String(), "restored  "+path+"\n") {
			t.Fatalf("fresh directory restored whole line missing for %s:\n%s", path, stdout.String())
		}
		if strings.Contains(stdout.String(), "already restored  "+path+"\n") {
			t.Fatalf("fresh directory falsely reported already restored for %s:\n%s", path, stdout.String())
		}
	}
}

func TestRestoreCommandDirectoryReplacedByFileFailsWithoutRestoredLine(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "child.txt"), "literal CLI child baseline\n")
	record := captureCLIRecord(t, stateDir, root, "literal-directory-file-replacement", func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, directory, "literal CLI replacement file\n")
	}, false)
	if len(record.Files.Changes) != 2 {
		t.Fatalf("replacement CLI fixture = %#v, want directory modification and child deletion", record.Files.Changes)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("replacement directory restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "restored  "+directory+"\n") ||
		strings.Contains(stdout.String(), "already restored  "+directory+"\n") {
		t.Fatalf("replacement file was reported as a restored directory:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "error     "+directory+": restore directory "+directory+": existing path has type") {
		t.Fatalf("replacement directory type failure was not reported as a whole path result:\n%s", stderr.String())
	}
	assertTestFile(t, directory, "literal CLI replacement file\n")
}

func TestRestoreCommandIncompleteNonEmptyDirectoryMergesObservedAndBaselineDescendants(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "archive", "2023")
	subdirectory := filepath.Join(directory, "sub")
	if err := os.MkdirAll(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(directory, "q1.csv")
	baselineOnly := filepath.Join(subdirectory, "r.csv")
	writeTestFile(t, observed, "literal q1 baseline\n")
	writeTestFile(t, baselineOnly, "literal r baseline\n")
	record, err := audit.NewRecord([]string{"literal-partial-directory-scan"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{root}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(observed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(baselineOnly); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(subdirectory, 0); err != nil {
		t.Fatal(err)
	}
	record.Files = session.Seal(time.Unix(200, 0))
	if err := os.Chmod(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if record.Files.Complete || len(record.Files.Changes) != 1 || record.Files.Changes[0].Path != observed {
		t.Fatalf("partial scan fixture = %#v, want one independently observed deletion and incomplete coverage", record.Files)
	}
	record.EndedAt = time.Unix(200, 0)
	record.Outcome = "discarded"
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("partial directory restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "restored  "+observed+"\n") {
		t.Fatalf("observed descendant whole line missing:\n%s", stdout.String())
	}
	if countCLIOutputLine(stdout.String(), "restored  "+observed) != 1 ||
		strings.Contains(stdout.String(), "already restored  "+observed+"\n") ||
		strings.Contains(stdout.String(), "recorded baseline written back  "+observed+" ") ||
		strings.Contains(stdout.String(), "recorded baseline already present  "+observed+" ") {
		t.Fatalf("observed descendant was not reported exactly once by the observed route:\n%s", stdout.String())
	}
	baselineLine := "recorded baseline written back  " + baselineOnly + " — change list incomplete; unring cannot confirm what the session did to this path\n"
	if !strings.Contains(stdout.String(), baselineLine) {
		t.Fatalf("baseline-only descendant whole line missing:\n%s", stdout.String())
	}
	for _, path := range []string{directory, subdirectory} {
		line := "recorded baseline already present  " + path + " — change list incomplete; unring cannot confirm what the session did to this path\n"
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("baseline directory whole line %q missing:\n%s", line, stdout.String())
		}
	}
	assertTestFile(t, observed, "literal q1 baseline\n")
	assertTestFile(t, baselineOnly, "literal r baseline\n")
}

func TestRestoreCommandForcedBaselineDirectoryReportsUnrecordedUserFileUntouched(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "baseline.txt"), "literal CLI baseline\n")
	record := captureCLIRecord(t, stateDir, root, "literal-force-unrecorded", func() {}, true)
	if record.Files.Complete || len(record.Files.Changes) != 0 {
		t.Fatalf("forced unrecorded fixture = %#v, want empty incomplete list", record.Files)
	}
	userFile := filepath.Join(directory, "later-user.txt")
	writeTestFile(t, userFile, "literal CLI later user bytes\n")

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", "--force", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("forced baseline directory exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	line := "left untouched  " + userFile + " — not in the recorded baseline; it may be later user work\n"
	if !strings.Contains(stdout.String(), line) {
		t.Fatalf("unrecorded user file whole line missing:\n%s", stdout.String())
	}
	assertTestFile(t, userFile, "literal CLI later user bytes\n")
}

func TestRestoreCommandReportsSkippedAgentStateDirectoryDescendants(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "scope")
	agentDirectory := filepath.Join(directory, ".agent-state")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(directory, "regular.txt")
	agentFile := filepath.Join(agentDirectory, "state.db")
	writeTestFile(t, regular, "literal CLI regular\n")
	writeTestFile(t, agentFile, "literal CLI agent state\n")
	record, err := audit.NewRecord([]string{"literal-agent-directory"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{
		Watched: []string{root}, AgentStateRoots: []string{agentDirectory},
	}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	record.Files = session.Seal(time.Unix(200, 0))
	if !record.Files.Complete || len(record.Files.Changes) != 4 {
		t.Fatalf("agent-state CLI fixture = %#v, want four deletions", record.Files)
	}
	record.EndedAt = time.Unix(200, 0)
	record.Outcome = "discarded"
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent-state parent restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, path := range []string{agentDirectory, agentFile} {
		line := "skipped   " + path + " — agent own-state is excluded unless that path or directory is explicitly named\n"
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("agent-state skipped whole line %q missing:\n%s", line, stdout.String())
		}
	}
	assertTestFile(t, regular, "literal CLI regular\n")
	if _, err := os.Lstat(agentFile); !os.IsNotExist(err) {
		t.Fatalf("skipped agent state exists: %v", err)
	}
}

func TestRestoreCommandLegacyManifestUsesInferredAgentStateRootsForExecution(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDirectory := filepath.Join(home, ".claude")
	if err := os.MkdirAll(agentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(home, "regular.txt")
	agentFile := filepath.Join(agentDirectory, "state.db")
	writeTestFile(t, regular, "literal legacy regular\n")
	writeTestFile(t, agentFile, "literal legacy agent\n")
	record, err := audit.NewRecord([]string{"literal-legacy-agent-directory"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{
		Watched: []string{home}, AgentStateRoots: []string{agentDirectory},
	}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(regular); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(agentDirectory); err != nil {
		t.Fatal(err)
	}
	record.Files = session.Seal(time.Unix(200, 0))
	if !record.Files.Complete || len(record.Files.Changes) != 3 {
		t.Fatalf("legacy fixture before downgrade = %#v, want three real deletions", record.Files)
	}
	record.Files.AgentStateRoots = nil
	record.EndedAt = time.Unix(200, 0)
	record.Outcome = "discarded"
	manifestPath := filepath.Join(stateDir, "snapshots", record.ID, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var legacyManifest map[string]json.RawMessage
	if err := json.Unmarshal(manifestBytes, &legacyManifest); err != nil {
		t.Fatal(err)
	}
	delete(legacyManifest, "agent_state_roots")
	manifestBytes, err = json.MarshalIndent(legacyManifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(manifestBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, home}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("legacy named-directory restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, path := range []string{agentDirectory, agentFile} {
		line := "skipped   " + path + " — agent own-state is excluded unless that path or directory is explicitly named\n"
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("legacy inferred agent-state whole line %q missing:\n%s", line, stdout.String())
		}
	}
	assertTestFile(t, regular, "literal legacy regular\n")
	if _, err := os.Lstat(agentFile); !os.IsNotExist(err) {
		t.Fatalf("legacy inferred agent state was restored: %v", err)
	}
}

func captureCLIRecord(t *testing.T, stateDir, root, command string, mutate func(), interrupted bool) audit.Record {
	t.Helper()
	record, err := audit.NewRecord([]string{command}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{root}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	mutate()
	if interrupted {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		record.Files = session.SealContext(ctx, time.Unix(200, 0), nil)
	} else {
		record.Files = session.Seal(time.Unix(200, 0))
	}
	record.EndedAt = time.Unix(200, 0)
	record.Outcome = "discarded"
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func assertUniqueCLIRestoreEvents(t *testing.T, events []localrollback.RestoreRecord, want int) {
	t.Helper()
	seen := make(map[string]bool)
	for _, event := range events {
		if seen[event.Path] {
			t.Fatalf("restore event path %s recorded twice in %#v", event.Path, events)
		}
		seen[event.Path] = true
	}
	if len(events) != want {
		t.Fatalf("restore events = %#v, want %d unique paths", events, want)
	}
}

func countCLIOutputLine(output, want string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == want {
			count++
		}
	}
	return count
}
