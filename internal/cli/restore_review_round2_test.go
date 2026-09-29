package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

func TestRestoreCommandResolvesRelativeSelectionOnceAcrossIncompleteRoutes(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	subdirectory := filepath.Join(root, "sub")
	gap := filepath.Join(root, "unreadable-gap")
	if err := os.MkdirAll(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(gap, "baseline.txt"), "literal gap baseline\n")
	record, err := audit.NewRecord([]string{"literal-relative-route"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{root}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(subdirectory, "new.txt")
	writeTestFile(t, created, "literal session-created bytes\n")
	if err := os.Chmod(gap, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gap, 0o700) })
	record.Files = session.Seal(time.Unix(200, 0))
	if err := os.Chmod(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	if record.Files.Complete || !summaryContainsChange(record.Files, created) {
		t.Fatalf("relative-route fixture = %#v, want incomplete list containing literal created path", record.Files)
	}
	saveCLIRecord(t, stateDir, &record)
	unrelated := filepath.Join(root, "new.txt")
	writeTestFile(t, unrelated, "literal unrelated user bytes\n")
	t.Chdir(root)

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", "--force", record.ID, "new.txt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("relative forced restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if countCLIOutputLine(stdout.String(), "restored  "+created) != 1 {
		t.Fatalf("resolved created path was not reported exactly once:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "already restored  "+created+"\n") {
		t.Fatalf("resolved created path was also reported already restored:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), unrelated) {
		t.Fatalf("unrelated cwd-relative path entered either restore route:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session-created selected path remains: %v", err)
	}
	assertTestFile(t, unrelated, "literal unrelated user bytes\n")
}

func TestRestoreCommandDoesNotReportUnringSidecarAsPossibleUserWork(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	gap := filepath.Join(root, "unreadable-gap")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "x.txt")
	writeTestFile(t, path, "literal sidecar baseline\n")
	writeTestFile(t, filepath.Join(gap, "baseline.txt"), "literal gap baseline\n")
	record, err := audit.NewRecord([]string{"literal-sidecar-reporting"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{root}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "literal session sidecar bytes\n")
	if err := os.Chmod(gap, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gap, 0o700) })
	record.Files = session.Seal(time.Unix(200, 0))
	if err := os.Chmod(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	if record.Files.Complete || !summaryContainsChange(record.Files, path) {
		t.Fatalf("sidecar fixture = %#v, want incomplete list containing literal modified path", record.Files)
	}
	saveCLIRecord(t, stateDir, &record)
	writeTestFile(t, path, "literal later user bytes\n")

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("conflicting sidecar restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	sidecars, err := filepath.Glob(path + ".unring-*.snapshot*")
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("sidecars = %#v, %v, want one preserved copy", sidecars, err)
	}
	line := "snapshot version written alongside: " + sidecars[0] + "\n"
	if !strings.Contains(stderr.String(), line) {
		t.Fatalf("sidecar result whole line %q missing:\n%s", line, stderr.String())
	}
	if strings.Contains(stdout.String(), "left untouched  "+sidecars[0]+"\n") ||
		strings.Contains(stdout.String(), "decision required  "+sidecars[0]+" ") {
		t.Fatalf("unring sidecar was presented as possible user work:\n%s", stdout.String())
	}
	assertTestFile(t, path, "literal later user bytes\n")
	assertTestFile(t, sidecars[0], "literal sidecar baseline\n")
}

func TestRestoreCommandParentOfWatchedRootReportsCurrentOnlyPaths(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	project := t.TempDir()
	watched := filepath.Join(project, "src")
	if err := os.MkdirAll(watched, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(watched, "baseline.txt"), "literal watched baseline\n")
	record, err := audit.NewRecord([]string{"literal-parent-selection"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{watched}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record.Files = session.SealContext(ctx, time.Unix(200, 0), nil)
	if record.Files.Complete || len(record.Files.Changes) != 0 {
		t.Fatalf("parent-selection fixture = %#v, want independently empty incomplete list", record.Files)
	}
	saveCLIRecord(t, stateDir, &record)
	currentOnly := filepath.Join(watched, "later.txt")
	writeTestFile(t, currentOnly, "literal current-only watched bytes\n")

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, project}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("unknown current-only path exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	leftLine := "left untouched  " + currentOnly + " — not in the recorded baseline; it may be later user work\n"
	decisionLine := "decision required  " + currentOnly + " — unring cannot tell whether the session or the user created this path; it was left in place; the user must decide what to do with it\n"
	for _, line := range []string{leftLine, decisionLine} {
		if !strings.Contains(stdout.String(), line) {
			t.Fatalf("current-only whole line %q missing:\n%s", line, stdout.String())
		}
	}
	assertTestFile(t, currentOnly, "literal current-only watched bytes\n")
}

func TestRestoreCommandSharedExecutionReportsCrossRouteDirectoryRestored(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	subdirectory := filepath.Join(directory, "sub")
	gap := filepath.Join(root, "unreadable-gap")
	if err := os.MkdirAll(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(subdirectory, "f.txt")
	writeTestFile(t, file, "literal cross-route baseline\n")
	writeTestFile(t, filepath.Join(gap, "baseline.txt"), "literal gap baseline\n")
	record, err := audit.NewRecord([]string{"literal-cross-route-directory"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{Watched: []string{root}}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gap, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gap, 0o700) })
	record.Files = session.Seal(time.Unix(200, 0))
	if err := os.Chmod(gap, 0o700); err != nil {
		t.Fatal(err)
	}
	if record.Files.Complete || len(record.Files.Changes) != 1 || record.Files.Changes[0].Path != file {
		t.Fatalf("cross-route fixture = %#v, want one independently observed deleted file", record.Files)
	}
	saveCLIRecord(t, stateDir, &record)
	if err := os.RemoveAll(subdirectory); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cross-route restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if countCLIOutputLine(stdout.String(), "restored  "+file) != 1 {
		t.Fatalf("observed file was not reported exactly once:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "already restored  "+file+"\n") {
		t.Fatalf("observed file was also reported already restored:\n%s", stdout.String())
	}
	restoredDirectoryLine := "recorded baseline written back  " + subdirectory + " — change list incomplete; unring cannot confirm what the session did to this path\n"
	if !strings.Contains(stdout.String(), restoredDirectoryLine) {
		t.Fatalf("cross-route directory restored line missing:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "recorded baseline already present  "+subdirectory+" ") {
		t.Fatalf("directory created by observed route was reported already present:\n%s", stdout.String())
	}
	rootDirectoryLine := "recorded baseline already present  " + directory + " — change list incomplete; unring cannot confirm what the session did to this path\n"
	if !strings.Contains(stdout.String(), rootDirectoryLine) {
		t.Fatalf("selected directory whole line missing:\n%s", stdout.String())
	}
	assertTestFile(t, file, "literal cross-route baseline\n")
}

func TestRestoreCommandDirectoryTypeErrorNamesSymlink(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	directory := filepath.Join(root, "D")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "child.txt"), "literal symlink type baseline\n")
	target := filepath.Join(root, "target.txt")
	writeTestFile(t, target, "literal symlink target bytes\n")
	record := captureCLIRecord(t, stateDir, root, "literal-symlink-type", func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, directory); err != nil {
			t.Fatal(err)
		}
	}, false)

	var stdout, stderr strings.Builder
	code := Main([]string{"restore", record.ID, directory}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("symlink directory replacement exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "it is a symlink, not a directory") {
		t.Fatalf("directory type error was not human-readable:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "restored  "+directory+"\n") ||
		strings.Contains(stdout.String(), "already restored  "+directory+"\n") {
		t.Fatalf("symlink replacement was reported as restored directory:\n%s", stdout.String())
	}
	link, err := os.Readlink(directory)
	if err != nil || link != target {
		t.Fatalf("replacement symlink = %q, %v, want untouched target %q", link, err, target)
	}
}

func summaryContainsChange(summary localrollback.Summary, path string) bool {
	for _, change := range summary.Changes {
		if change.Path == path {
			return true
		}
	}
	return false
}

func saveCLIRecord(t *testing.T, stateDir string, record *audit.Record) {
	t.Helper()
	record.EndedAt = time.Unix(200, 0)
	record.Outcome = "discarded"
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(*record); err != nil {
		t.Fatal(err)
	}
}
