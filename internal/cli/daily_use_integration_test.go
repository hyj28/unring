package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/httpsproxy"
)

func TestFileChangesAreRecordedListedAndRestoredIndividually(t *testing.T) {
	stateDir := t.TempDir()
	watched := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)

	writeTestFile(t, filepath.Join(watched, "modified-safe.txt"), "SAFE-BEFORE")
	writeTestFile(t, filepath.Join(watched, "modified-conflict.txt"), "conflict-before")
	writeTestFile(t, filepath.Join(watched, "deleted.txt"), "deleted-before")
	writeTestFile(t, filepath.Join(watched, "untouched.txt"), "untouched")
	reference := filepath.Join(watched, "mtime-reference")
	writeTestFile(t, reference, "reference")
	originalTime := time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.Local)
	for _, path := range []string{
		filepath.Join(watched, "modified-safe.txt"),
		filepath.Join(watched, "modified-conflict.txt"),
		reference,
	} {
		if err := os.Chtimes(path, originalTime, originalTime); err != nil {
			t.Fatalf("set literal original mtime: %v", err)
		}
	}

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", watched, "--", "/bin/sh", "-c", `
printf 'SAFE--AFTER' > modified-safe.txt
touch -r mtime-reference modified-safe.txt
printf 'conflict--after' > modified-conflict.txt
touch -r mtime-reference modified-conflict.txt
printf 'created-by-child' > created.txt
rm deleted.txt
exit 23
`)
	command.Dir = watched
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
		t.Fatalf("file-changing child exit = %v, want 23\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "Files changed: 1 created, 2 modified, 1 deleted.") {
		t.Fatalf("file summary omitted literal change counts:\n%s", text)
	}
	for _, unwanted := range []string{"Commit or discard?", "Up/down: select"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("file-only session prompted with %q:\n%s", unwanted, text)
		}
	}

	sessionID := newestSessionID(t, binary)
	logCommand := exec.Command(binary, "log", "--json", sessionID)
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read file-session JSON log: %v\n%s", err, logOutput)
	}
	for _, want := range []string{
		`"kind": "created"`, `"kind": "modified"`, `"kind": "deleted"`,
		`"path": "` + filepath.Join(watched, "created.txt") + `"`,
		`"path": "` + filepath.Join(watched, "modified-safe.txt") + `"`,
		`"path": "` + filepath.Join(watched, "deleted.txt") + `"`,
	} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("file-session JSON missing %q:\n%s", want, logOutput)
		}
	}

	listing := exec.Command(binary, "restore", sessionID)
	listing.Env = os.Environ()
	listOutput, err := listing.CombinedOutput()
	if err != nil {
		t.Fatalf("list restorable changes: %v\n%s", err, listOutput)
	}
	for _, want := range []string{"created", "modified", "deleted", "created.txt", "modified-safe.txt", "deleted.txt"} {
		if !strings.Contains(string(listOutput), want) {
			t.Fatalf("restore listing missing %q:\n%s", want, listOutput)
		}
	}

	restore := exec.Command(binary, "restore", sessionID, "modified-safe.txt", "deleted.txt")
	restore.Dir = watched
	restore.Env = os.Environ()
	restoreOutput, err := restore.CombinedOutput()
	if err != nil {
		t.Fatalf("restore selected files: %v\n%s", err, restoreOutput)
	}
	assertTestFile(t, filepath.Join(watched, "modified-safe.txt"), "SAFE-BEFORE")
	assertTestFile(t, filepath.Join(watched, "deleted.txt"), "deleted-before")
	assertTestFile(t, filepath.Join(watched, "created.txt"), "created-by-child")
	assertTestFile(t, filepath.Join(watched, "untouched.txt"), "untouched")
	assertTestFile(t, filepath.Join(watched, "modified-conflict.txt"), "conflict--after")

	writeTestFile(t, filepath.Join(watched, "modified-conflict.txt"), "changed-after-session")
	conflictingRestore := exec.Command(binary, "restore", sessionID, "modified-conflict.txt")
	conflictingRestore.Dir = watched
	conflictingRestore.Env = os.Environ()
	conflictOutput, err := conflictingRestore.CombinedOutput()
	if err == nil {
		t.Fatalf("conflicting restore unexpectedly succeeded:\n%s", conflictOutput)
	}
	conflictText := string(conflictOutput)
	for _, want := range []string{"refused", "modified-conflict.txt", "changed after the session ended", "--force"} {
		if !strings.Contains(conflictText, want) {
			t.Fatalf("conflict refusal missing %q:\n%s", want, conflictText)
		}
	}
	assertTestFile(t, filepath.Join(watched, "modified-conflict.txt"), "changed-after-session")
	sidecars, err := filepath.Glob(filepath.Join(watched, "modified-conflict.txt.unring-*.snapshot"))
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("conflict sidecars = %v, %v, want exactly one", sidecars, err)
	}
	assertTestFile(t, sidecars[0], "conflict-before")

	forcedRestore := exec.Command(binary, "restore", "--force", sessionID, "modified-conflict.txt")
	forcedRestore.Dir = watched
	forcedRestore.Env = os.Environ()
	forcedOutput, err := forcedRestore.CombinedOutput()
	if err != nil {
		t.Fatalf("forced restore: %v\n%s", err, forcedOutput)
	}
	assertTestFile(t, filepath.Join(watched, "modified-conflict.txt"), "conflict-before")

	removeCreated := exec.Command(binary, "restore", sessionID, "created.txt")
	removeCreated.Dir = watched
	removeCreated.Env = os.Environ()
	removeOutput, err := removeCreated.CombinedOutput()
	if err != nil {
		t.Fatalf("restore selected created file to absence: %v\n%s", err, removeOutput)
	}
	if _, err := os.Stat(filepath.Join(watched, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("restoring a created file did not remove it: %v", err)
	}
}

func TestRestoreAllRestoresEveryChangedPathIncludingDirectories(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	modified := filepath.Join(root, "modified.txt")
	deleted := filepath.Join(root, "deleted.txt")
	writeTestFile(t, modified, "before-modified")
	writeTestFile(t, deleted, "before-deleted")

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", root, "--", "/bin/sh", "-c",
		"printf 'after-modified' > modified.txt; rm deleted.txt; mkdir empty-created; printf 'created' > created.txt")
	command.Dir = root
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run file changes: %v\n%s", err, output)
	}
	sessionID := newestSessionID(t, binary)
	restore := exec.Command(binary, "restore", "--all", sessionID)
	restore.Env = os.Environ()
	output, err := restore.CombinedOutput()
	if err != nil {
		t.Fatalf("restore --all: %v\n%s", err, output)
	}
	for _, path := range []string{modified, deleted, filepath.Join(root, "created.txt"), filepath.Join(root, "empty-created")} {
		if !strings.Contains(string(output), path) {
			t.Fatalf("restore --all output omitted %s:\n%s", path, output)
		}
	}
	assertTestFile(t, modified, "before-modified")
	assertTestFile(t, deleted, "before-deleted")
	for _, path := range []string{filepath.Join(root, "created.txt"), filepath.Join(root, "empty-created")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created path %s remains after --all: %v", path, err)
		}
	}
}

func TestUnrestorableCreatedPathIsListedWithoutBlockingProtectedRestore(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	target := t.TempDir()
	protected := filepath.Join(root, "protected.txt")
	linked := filepath.Join(root, "linked-build-output")
	writeTestFile(t, protected, "before")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", root, "--", "/bin/sh", "-c",
		"rm protected.txt; ln -s \"$1\" linked-build-output", "unring-test", target)
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run beside newly unprotectable path: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{protected, linked, "created", "deleted", "NOT RESTORABLE", "Restore later with:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("session output missing %q:\n%s", want, text)
		}
	}

	sessionID := newestSessionID(t, binary)
	logCommand := exec.Command(binary, "log", "--json", sessionID)
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read scoped-coverage audit: %v\n%s", err, logOutput)
	}
	for _, want := range []string{`"path": "` + linked + `"`, `"unrestorable_reason":`, `"uncaptured_paths": [`} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("scoped-coverage audit missing %q:\n%s", want, logOutput)
		}
	}
	listing := exec.Command(binary, "restore", sessionID)
	listing.Env = os.Environ()
	listingOutput, err := listing.CombinedOutput()
	if err != nil {
		t.Fatalf("list partial restore: %v\n%s", err, listingOutput)
	}
	for _, want := range []string{protected, linked, "NOT RESTORABLE", "Restore every restorable path"} {
		if !strings.Contains(string(listingOutput), want) {
			t.Fatalf("restore listing missing %q:\n%s", want, listingOutput)
		}
	}

	restore := exec.Command(binary, "restore", "--all", sessionID)
	restore.Env = os.Environ()
	restoreOutput, err := restore.CombinedOutput()
	if err == nil {
		t.Fatalf("restore --all did not report unavailable path:\n%s", restoreOutput)
	}
	for _, want := range []string{"restored  " + protected, "unavailable " + linked, "outside snapshot coverage"} {
		if !strings.Contains(string(restoreOutput), want) {
			t.Fatalf("partial restore output missing %q:\n%s", want, restoreOutput)
		}
	}
	assertTestFile(t, protected, "before")
	if _, err := os.Lstat(linked); err != nil {
		t.Fatalf("unrestorable path was unexpectedly changed: %v", err)
	}
}

func TestSymlinkedWatchRootIsProtectedAndNestedSymlinkIsDisclosed(t *testing.T) {
	stateDir := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "Documents")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(target, "taxes.pdf")
	writeTestFile(t, protected, "taxes-before")
	nestedTarget := t.TempDir()
	nested := filepath.Join(target, "external-data")
	if err := os.Symlink(nestedTarget, nested); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", link, "--", "/bin/rm", filepath.Join(link, "taxes.pdf"))
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("symlink-root run: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{"FILE NOT SNAPSHOTTED", filepath.Join(link, "external-data"), filepath.Join(link, "taxes.pdf"), "deleted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("symlink coverage output missing %q:\n%s", want, text)
		}
	}
	sessionID := newestSessionID(t, binary)
	restore := exec.Command(binary, "restore", "--all", sessionID)
	restore.Env = os.Environ()
	if restoreOutput, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore symlink-root file: %v\n%s", err, restoreOutput)
	}
	assertTestFile(t, protected, "taxes-before")
}

func TestRestoreRefusesAWatchRootRetargetEvenWithForce(t *testing.T) {
	stateDir := t.TempDir()
	firstTarget := t.TempDir()
	secondTarget := t.TempDir()
	root := filepath.Join(t.TempDir(), "Documents")
	if err := os.Symlink(firstTarget, root); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(firstTarget, "notes.txt")
	logical := filepath.Join(root, "notes.txt")
	writeTestFile(t, original, "secret")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)

	run := exec.Command(binary, "run", "--watch-only", root, "--", "/bin/rm", logical)
	run.Env = os.Environ()
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("delete through original watched root: %v\n%s", err, output)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondTarget, root); err != nil {
		t.Fatal(err)
	}

	sessionID := newestSessionID(t, binary)
	restore := exec.Command(binary, "restore", "--force", sessionID, logical)
	restore.Env = os.Environ()
	output, err := restore.CombinedOutput()
	if err == nil {
		t.Fatalf("restore through retargeted watched root unexpectedly succeeded:\n%s", output)
	}
	for _, want := range []string{"refused", root, secondTarget, firstTarget, "not restored"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("retarget refusal missing %q:\n%s", want, output)
		}
	}
	if _, err := os.Lstat(filepath.Join(secondTarget, "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore wrote into the new watched-root target: %v", err)
	}
	if _, err := os.Lstat(original); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore unexpectedly changed the original target: %v", err)
	}
}

func TestMissingWatchedRootRefusesStartupAndIsRecordedInAudit(t *testing.T) {
	stateDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "Dcouments")
	marker := filepath.Join(t.TempDir(), "child-ran")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", missing, "--",
		"/bin/sh", "-c", `printf ran > "$1"`, "unring-test", marker)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != usageExitCode {
		t.Fatalf("missing-watch exit = %v, want %d\n%s", err, usageExitCode, output)
	}
	for _, want := range []string{missing, "does not exist"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("missing root refusal omitted %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing watched root launched child: %v", err)
	}
	logCommand := exec.Command(binary, "log", "--json", newestSessionID(t, binary))
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("missing-watch audit: %v\n%s", err, logOutput)
	}
	for _, want := range []string{
		`"outcome": "not_started"`, `"path": "` + missing + `"`,
		`"error": "watched path does not exist"`,
	} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("missing root audit omitted %q:\n%s", want, logOutput)
		}
	}
}

func TestConfigurableSnapshotScopeThroughCLI(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	binary := buildTestBinary(t)
	home := os.Getenv("HOME")

	run := func(t *testing.T, directory string, args ...string) (string, string, error) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Dir = directory
		command.Env = os.Environ()
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	makeProject := func(t *testing.T) string {
		t.Helper()
		project := t.TempDir()
		if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		return project
	}

	t.Run("default config and flag watches are additive and exclusions subtract", func(t *testing.T) {
		stateDir := t.TempDir()
		t.Setenv("UNRING_STATE_DIR", stateDir)
		project := makeProject(t)
		configWatch := filepath.Join(home, "config-watch-additive")
		flagWatch := t.TempDir()
		for _, directory := range []string{
			configWatch,
			filepath.Join(project, "private"),
			filepath.Join(configWatch, "private"),
		} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		defaultFile := filepath.Join(project, "default.txt")
		defaultExcluded := filepath.Join(project, "private", "excluded.txt")
		configFile := filepath.Join(configWatch, "configured.txt")
		configExcluded := filepath.Join(configWatch, "private", "excluded.txt")
		flagFile := filepath.Join(flagWatch, "flag.txt")
		for path, content := range map[string]string{
			defaultFile: "default", defaultExcluded: "default-excluded",
			configFile: "configured", configExcluded: "config-excluded", flagFile: "flag",
		} {
			writeTestFile(t, path, content)
		}
		config := "watch:\n  - ~/config-watch-additive\nexclude:\n  - " +
			filepath.Join(project, "private") + "\n  - ~/config-watch-additive/private\n"
		if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, stderr, err := run(t, project,
			"run", "--watch", flagWatch, "--", "/bin/rm",
			defaultFile, defaultExcluded, configFile, configExcluded, flagFile,
		)
		if err != nil {
			t.Fatalf("additive run: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		for _, path := range []string{defaultFile, configFile, flagFile} {
			if !strings.Contains(stdout, path) {
				t.Fatalf("additive change report omitted %s:\n%s", path, stdout)
			}
		}
		for _, path := range []string{defaultExcluded, configExcluded} {
			if strings.Contains(stdout, path) {
				t.Fatalf("change report included excluded path %s:\n%s", path, stdout)
			}
		}

		sessionID := newestSessionID(t, binary)
		restoreStdout, restoreStderr, err := run(t, project, "restore", "--all", sessionID)
		if err != nil {
			t.Fatalf("restore additive scope: %v\nstdout: %s\nstderr: %s", err, restoreStdout, restoreStderr)
		}
		assertTestFile(t, defaultFile, "default")
		assertTestFile(t, configFile, "configured")
		assertTestFile(t, flagFile, "flag")
		for _, path := range []string{defaultExcluded, configExcluded} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("excluded path %s was restored: %v", path, err)
			}
		}
	})

	t.Run("watch only replaces defaults and config watch but keeps exclusions", func(t *testing.T) {
		stateDir := t.TempDir()
		t.Setenv("UNRING_STATE_DIR", stateDir)
		project := makeProject(t)
		configured := t.TempDir()
		only := t.TempDir()
		if err := os.Mkdir(filepath.Join(only, "private"), 0o700); err != nil {
			t.Fatal(err)
		}
		defaultFile := filepath.Join(project, "default-only-test.txt")
		configuredFile := filepath.Join(configured, "configured-only-test.txt")
		onlyFile := filepath.Join(only, "only.txt")
		excludedFile := filepath.Join(only, "private", "excluded-only-test.txt")
		for path, content := range map[string]string{
			defaultFile: "default", configuredFile: "configured", onlyFile: "only", excludedFile: "excluded",
		} {
			writeTestFile(t, path, content)
		}
		config := "watch:\n  - " + configured + "\nexclude:\n  - " + filepath.Join(only, "private") + "\n"
		if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}

		stdout, stderr, err := run(t, project,
			"run", "--watch-only", only, "--", "/bin/rm",
			defaultFile, configuredFile, onlyFile, excludedFile,
		)
		if err != nil {
			t.Fatalf("watch-only run: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if !strings.Contains(stdout, onlyFile) {
			t.Fatalf("watch-only report omitted %s:\n%s", onlyFile, stdout)
		}
		for _, path := range []string{defaultFile, configuredFile, excludedFile} {
			if strings.Contains(stdout, path) {
				t.Fatalf("watch-only report included out-of-scope path %s:\n%s", path, stdout)
			}
		}

		sessionID := newestSessionID(t, binary)
		if stdout, stderr, err := run(t, project, "restore", "--all", sessionID); err != nil {
			t.Fatalf("restore watch-only scope: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		assertTestFile(t, onlyFile, "only")
		for _, path := range []string{defaultFile, configuredFile, excludedFile} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("out-of-scope path %s was restored: %v", path, err)
			}
		}
	})

	t.Run("watch and watch only are a usage error", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "child-ran")
		stdout, stderr, err := run(t, t.TempDir(),
			"run", "--watch", t.TempDir(), "--watch-only", t.TempDir(), "--",
			"/bin/sh", "-c", "touch \"$1\"", "unring-test", marker,
		)
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != usageExitCode {
			t.Fatalf("mutually exclusive watch exit = %v, want %d\nstdout: %s\nstderr: %s", err, usageExitCode, stdout, stderr)
		}
		if !strings.Contains(stderr, "--watch and --watch-only are mutually exclusive") {
			t.Fatalf("mutual-exclusion error was not actionable: %s", stderr)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("usage error launched child: %v", err)
		}
	})

	t.Run("missing defaults are silent and missing explicit paths refuse startup", func(t *testing.T) {
		project := makeProject(t)
		stateDir := t.TempDir()
		t.Setenv("UNRING_STATE_DIR", stateDir)
		stdout, stderr, err := run(t, project, "run", "--", "/usr/bin/true")
		if err != nil {
			t.Fatalf("default-scope run: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		if strings.Contains(stderr, "does not exist") || strings.Contains(stderr, "scope config") {
			t.Fatalf("missing default or missing config produced output: %s", stderr)
		}

		for _, test := range []struct {
			name string
			args func(string, string) []string
		}{
			{name: "watch", args: func(path, marker string) []string {
				return []string{"run", "--watch", path, "--", "/bin/sh", "-c", `printf ran > "$1"`, "unring-test", marker}
			}},
			{name: "watch-only", args: func(path, marker string) []string {
				return []string{"run", "--watch-only", path, "--", "/bin/sh", "-c", `printf ran > "$1"`, "unring-test", marker}
			}},
			{name: "config", args: func(path, marker string) []string {
				if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("watch:\n  - "+path+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"run", "--", "/bin/sh", "-c", `printf ran > "$1"`, "unring-test", marker}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				if err := os.Remove(filepath.Join(stateDir, "config.yaml")); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				missing := filepath.Join(t.TempDir(), "missing-explicit")
				marker := filepath.Join(t.TempDir(), "child-ran")
				stdout, stderr, err := run(t, project, test.args(missing, marker)...)
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) || exitError.ExitCode() != usageExitCode {
					t.Fatalf("missing explicit path exit = %v, want %d\nstdout: %s\nstderr: %s", err, usageExitCode, stdout, stderr)
				}
				for _, want := range []string{missing, "does not exist"} {
					if !strings.Contains(stderr, want) {
						t.Fatalf("missing explicit refusal omitted %q: %s", want, stderr)
					}
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing explicit path launched child: %v", err)
				}
			})
		}
	})

	t.Run("invalid config names the file and prevents child startup", func(t *testing.T) {
		project := makeProject(t)
		for _, test := range []struct {
			name       string
			contents   string
			wantDetail string
		}{
			{name: "malformed YAML", contents: "watch: [\n", wantDetail: "decode YAML"},
			{name: "relative path", contents: "watch:\n  - relative/path\n", wantDetail: "must be absolute"},
		} {
			t.Run(test.name, func(t *testing.T) {
				stateDir := t.TempDir()
				t.Setenv("UNRING_STATE_DIR", stateDir)
				filename := filepath.Join(stateDir, "config.yaml")
				if err := os.WriteFile(filename, []byte(test.contents), 0o600); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(t.TempDir(), "child-ran")
				stdout, stderr, err := run(t, project,
					"run", "--", "/bin/sh", "-c", "touch \"$1\"", "unring-test", marker,
				)
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) || exitError.ExitCode() != usageExitCode {
					t.Fatalf("invalid config exit = %v, want %d\nstdout: %s\nstderr: %s", err, usageExitCode, stdout, stderr)
				}
				for _, want := range []string{filename, test.wantDetail} {
					if !strings.Contains(stderr, want) {
						t.Fatalf("config error omitted %q: %s", want, stderr)
					}
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid config launched child: %v", err)
				}
			})
		}
	})
}

func TestExplicitWatchExcludedByConfigIsReportedThroughCLI(t *testing.T) {
	stateDir := t.TempDir()
	project := t.TempDir()
	private := filepath.Join(project, "private")
	keep := filepath.Join(private, "keep")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	important := filepath.Join(keep, "important.txt")
	secret := "PRIVATEKEY-CONTENTS-ROUND2"
	writeTestFile(t, important, secret)
	configFile := filepath.Join(stateDir, "config.yaml")
	if err := os.WriteFile(configFile, []byte("exclude:\n  - "+private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)

	command := exec.Command(binary, "run", "--watch-only", keep, "--", "/bin/rm", important)
	command.Dir = project
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("excluded explicit watch run: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"FILE NOT SNAPSHOTTED", keep, "excluded"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("excluded explicit watch warning omitted %q:\n%s", want, stderr.String())
		}
	}

	sessionID := newestSessionID(t, binary)
	logCommand := exec.Command(binary, "log", "--json", sessionID)
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read excluded-watch audit: %v\n%s", err, logOutput)
	}
	for _, want := range []string{`"path": "` + keep + `"`, "excluded"} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("excluded explicit watch audit omitted %q:\n%s", want, logOutput)
		}
	}
	if err := filepath.WalkDir(filepath.Join(stateDir, "snapshots"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("config-excluded contents were copied into snapshot store at %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect snapshot store for excluded contents: %v", err)
	}
}

func TestWatchOnlyDoesNotRequireHomeAndScopeFailuresKeepInternalExitCode(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	binary := buildTestBinary(t)
	withoutHome := func() []string {
		var environment []string
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "HOME=") {
				environment = append(environment, entry)
			}
		}
		return environment
	}

	watchOnlyState := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", watchOnlyState)
	if err := os.WriteFile(
		filepath.Join(watchOnlyState, "config.yaml"),
		[]byte("watch:\n  - ~/discarded-by-watch-only\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	watchOnly := exec.Command(binary, "run", "--watch-only", t.TempDir(), "--", "/usr/bin/true")
	watchOnly.Env = withoutHome()
	if output, err := watchOnly.CombinedOutput(); err != nil {
		t.Fatalf("watch-only with HOME unset: %v\n%s", err, output)
	}

	defaultState := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", defaultState)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	defaultRun := exec.Command(binary, "run", "--", "/usr/bin/true")
	defaultRun.Dir = project
	defaultRun.Env = withoutHome()
	output, err := defaultRun.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != internalErrorExitCode {
		t.Fatalf("default scope without HOME exit = %v, want %d\n%s", err, internalErrorExitCode, output)
	}
	if !strings.Contains(string(output), "find home directory for default snapshot scope") {
		t.Fatalf("internal scope error omitted cause:\n%s", output)
	}

	homeExcludeState := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", homeExcludeState)
	if err := os.WriteFile(
		filepath.Join(homeExcludeState, "config.yaml"),
		[]byte("exclude:\n  - ~/Pictures\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	homeExclude := exec.Command(binary, "run", "--watch-only", t.TempDir(), "--", "/usr/bin/true")
	homeExclude.Env = withoutHome()
	output, err = homeExclude.CombinedOutput()
	if !errors.As(err, &exitError) || exitError.ExitCode() != internalErrorExitCode {
		t.Fatalf("config exclusion without HOME exit = %v, want %d\n%s", err, internalErrorExitCode, output)
	}
	if !strings.Contains(string(output), "$HOME is not defined") {
		t.Fatalf("config exclusion HOME error omitted cause:\n%s", output)
	}

	readErrorState := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", readErrorState)
	if err := os.Mkdir(filepath.Join(readErrorState, "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	readError := exec.Command(binary, "run", "--watch-only", t.TempDir(), "--", "/usr/bin/true")
	readError.Env = os.Environ()
	output, err = readError.CombinedOutput()
	if !errors.As(err, &exitError) || exitError.ExitCode() != internalErrorExitCode {
		t.Fatalf("config read error exit = %v, want %d\n%s", err, internalErrorExitCode, output)
	}
	for _, want := range []string{"read snapshot scope config", "config.yaml"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("config read error omitted %q:\n%s", want, output)
		}
	}
}

func TestDanglingDefaultSymlinkIsDisclosedThroughCLI(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	binary := buildTestBinary(t)
	home := os.Getenv("HOME")
	documents := filepath.Join(home, "Documents")
	if err := os.Symlink(filepath.Join(t.TempDir(), "offline-documents"), documents); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "run", "--", "/usr/bin/true")
	command.Dir = project
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("dangling default run: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"FILE NOT SNAPSHOTTED", documents, "does not exist"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("dangling default disclosure omitted %q:\n%s", want, stderr.String())
		}
	}
}

func TestCurrentSnapshotOverCapRemainsRestorable(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeTestFile(t, file, "before")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--snapshot-cap-bytes", "1", "--watch-only", root, "--", "/bin/sh", "-c", "printf after! > file")
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("over-cap current session: %v\n%s", err, output)
	}
	text := string(output)
	sessionID := newestSessionID(t, binary)
	for _, want := range []string{"current snapshot", "remains retained", "Restore later with: unring restore " + sessionID} {
		if !strings.Contains(text, want) {
			t.Fatalf("over-cap session output omitted %q:\n%s", want, text)
		}
	}
	restore := exec.Command(binary, "restore", sessionID, file)
	restore.Env = os.Environ()
	if restoreOutput, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore retained over-cap current snapshot: %v\n%s", err, restoreOutput)
	}
	assertTestFile(t, file, "before")
}

func TestUnsnapshottedPathIsReportedAtStartAndInSessionRecord(t *testing.T) {
	stateDir := t.TempDir()
	watched := t.TempDir()
	unreadable := filepath.Join(watched, "unreadable.txt")
	writeTestFile(t, unreadable, "not-readable")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatalf("make snapshot reproducer unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", watched, "--", "/usr/bin/true")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run with partially unreadable tree: %v\n%s", err, output)
	}
	for _, want := range []string{"FILE NOT SNAPSHOTTED", unreadable} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("start warning missing %q:\n%s", want, output)
		}
	}

	sessionID := newestSessionID(t, binary)
	logCommand := exec.Command(binary, "log", "--json", sessionID)
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read incomplete-snapshot JSON: %v\n%s", err, logOutput)
	}
	for _, want := range []string{`"uncaptured_paths": [`, `"path": "` + unreadable + `"`} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("session record missing %q:\n%s", want, logOutput)
		}
	}
}

func TestSnapshotRetentionEvictsOldestAndReportsUsage(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)

	firstWatch := t.TempDir()
	for index := 0; index < 128; index++ {
		writeTestFile(t, filepath.Join(firstWatch, fmt.Sprintf("first-retained-file-%03d", index)), "12345678")
	}
	first := exec.Command(binary, "run", "--snapshot-cap-bytes", "12000", "--watch-only", firstWatch, "--", "/usr/bin/true")
	first.Env = os.Environ()
	if output, err := first.CombinedOutput(); err != nil {
		t.Fatalf("first retained session: %v\n%s", err, output)
	}
	firstID := newestSessionID(t, binary)
	time.Sleep(time.Millisecond)

	secondWatch := t.TempDir()
	for index := 0; index < 128; index++ {
		writeTestFile(t, filepath.Join(secondWatch, fmt.Sprintf("second-retained-file-%03d", index)), "abcdefgh")
	}
	second := exec.Command(binary, "run", "--snapshot-cap-bytes", "12000", "--watch-only", secondWatch, "--", "/usr/bin/true")
	second.Env = os.Environ()
	secondOutput, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second retained session: %v\n%s", err, secondOutput)
	}
	if !strings.Contains(string(secondOutput), "retention evicted oldest snapshot "+firstID) {
		t.Fatalf("retention output did not name oldest session:\n%s", secondOutput)
	}

	usage := exec.Command(binary, "snapshots")
	usage.Env = os.Environ()
	usageOutput, err := usage.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect snapshot usage: %v\n%s", err, usageOutput)
	}
	if want := "bytes used of 12000 bytes; 1 sessions retained."; !strings.Contains(string(usageOutput), want) {
		t.Fatalf("snapshot usage missing %q:\n%s", want, usageOutput)
	}

	oldLog := exec.Command(binary, "log", "--json", firstID)
	oldLog.Env = os.Environ()
	oldOutput, err := oldLog.CombinedOutput()
	if err != nil {
		t.Fatalf("read evicted session record: %v\n%s", err, oldOutput)
	}
	if !strings.Contains(string(oldOutput), `"retained": false`) {
		t.Fatalf("evicted audit record still claims retention:\n%s", oldOutput)
	}
}

func TestSnapshotsReportsTheCapAnExplicitRunEnforced(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("UNRING_SNAPSHOT_CAP_BYTES", "1000")
	binary := buildTestBinary(t)

	run := exec.Command(binary, "run", "--snapshot-cap-bytes", "5000", "--watch-only", t.TempDir(), "--", "/usr/bin/true")
	run.Env = os.Environ()
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("run with explicit cap: %v\n%s", err, output)
	}
	usage := exec.Command(binary, "snapshots")
	usage.Env = os.Environ()
	output, err := usage.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect persisted explicit cap: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "of 5000 bytes") || strings.Contains(string(output), "of 1000 bytes") {
		t.Fatalf("snapshot usage did not report the enforced 5000-byte cap:\n%s", output)
	}
}

func TestOutboundInterceptionIsOffByDefault(t *testing.T) {
	stateDir := t.TempDir()
	watched := t.TempDir()
	fakeDirectory := t.TempDir()
	realGHMarker := filepath.Join(t.TempDir(), "real-gh-ran")
	fakeGH := filepath.Join(fakeDirectory, "gh")
	if err := os.WriteFile(fakeGH, []byte(
		"#!/bin/sh\nprintf 'REAL GH RAN\\n'\nprintf ran > \""+realGHMarker+"\"\n",
	), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HTTPS_PROXY", "http://inherited-proxy.invalid:4321")

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--watch-only", watched, "--", "/bin/sh", "-c",
		`printf 'proxy=<%s> shim=<%s>\n' "$HTTPS_PROXY" "$UNRING_GH_SHIM"; gh issue create`)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("default outbound-off run: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{"REAL GH RAN", "proxy=<http://inherited-proxy.invalid:4321>", "shim=<>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("outbound-off output missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"gh action needs approval", "HTTPS action needs approval", "No interactive terminal; declining"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("outbound-off session unexpectedly prompted with %q:\n%s", unwanted, text)
		}
	}
	if _, err := os.Stat(realGHMarker); err != nil {
		t.Fatalf("real gh was not run with shim disabled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ca")); !os.IsNotExist(err) {
		t.Fatalf("outbound-off session created HTTPS CA/proxy state: %v", err)
	}

	sessionID := newestSessionID(t, binary)
	logCommand := exec.Command(binary, "log", "--json", sessionID)
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read outbound-off JSON: %v\n%s", err, logOutput)
	}
	if !strings.Contains(string(logOutput), `"outbound_enabled": false`) {
		t.Fatalf("audit did not record outbound disabled:\n%s", logOutput)
	}
}

func newestSessionID(t *testing.T, binary string) string {
	t.Helper()
	command := exec.Command(binary, "log", "--json")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list JSON sessions: %v\n%s", err, output)
	}
	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &records); err != nil || len(records) == 0 {
		t.Fatalf("decode JSON sessions: %v\n%s", err, output)
	}
	return records[0].ID
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := string(data); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func TestRunWithoutDatabaseRunsChildPropagatesExitAndRecordsCoverage(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--watch-only", t.TempDir(),
		"--",
		"/bin/sh",
		"-c",
		"printf 'child-ran-without-database\\n'; exit 23",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
		t.Fatalf("database-free run exit = %v, want 23\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"child-ran-without-database",
		"UNRING SESSION REVIEW",
		"NOT INTERCEPTED — no database traffic was intercepted",
		"not evidence that the child did not access a database",
		"git push over SSH",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free review missing %q:\n%s", want, text)
		}
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	for _, want := range []string{
		`"interception_status": "not_configured"`,
		`"exit_code": 23`,
		`"structural_blind_spots"`,
		`git push over SSH`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("database-free JSON audit missing %q:\n%s", want, logText)
		}
	}
}

func TestRunWithoutDatabaseDirectGHMutationIsGatedByShim(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	stateDir := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	runLog := filepath.Join(t.TempDir(), "real-gh-ran")
	fakeDirectory := t.TempDir()
	fakeGH := filepath.Join(fakeDirectory, "gh")
	if err := os.WriteFile(fakeGH, []byte(
		"#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+runLog+"\"\nprintf 'REAL GH RAN\\n'\n",
	), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"--outbound",
		"gh",
		"issue",
		"create",
		"--title",
		"database-free shim regression",
		"--body",
		"must be gated",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("declined direct gh mutation exit = %v, want nonzero\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"gh action needs approval",
		"No interactive terminal; declining the action",
		"GH INVOCATIONS — MUTATIONS AND AMBIGUOUS COMMANDS",
		"[not-run] gh issue create",
		"NOT INTERCEPTED — no database traffic was intercepted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free direct gh review missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "REAL GH RAN") {
		t.Fatalf("declined direct gh mutation ran the parent-PATH executable:\n%s", text)
	}
	if _, err := os.Stat(runLog); !os.IsNotExist(err) {
		t.Fatalf("declined direct gh mutation invoked real gh: %v", err)
	}
	if strings.Contains(text, "Query batches: 0") {
		t.Fatalf("database-free review implied zero observed PostgreSQL traffic:\n%s", text)
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	for _, want := range []string{
		`"arguments": [`,
		`"issue"`,
		`"create"`,
		`"state": "not-run"`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("direct gh JSON audit missing %q:\n%s", want, logText)
		}
	}
}

func TestRunWithoutDatabaseStillInterceptsHTTPS(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	binary := buildTestBinary(t)
	command := exec.Command(
		binary,
		"run",
		"--discard",
		"--outbound",
		"--watch-only", t.TempDir(),
		"--",
		curl,
		"--silent",
		"--show-error",
		"--max-time",
		"5",
		"--request",
		"POST",
		"--data",
		"must-not-be-sent",
		"https://127.0.0.1:1/unring-no-database",
	)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("database-free HTTPS interception failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"HTTPS action needs approval",
		"No interactive terminal; declining the action",
		"HTTPS APPROVALS — NOT SENT",
		"POST https://127.0.0.1:1/unring-no-database",
		"NOT INTERCEPTED — no database traffic was intercepted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("database-free HTTPS review missing %q:\n%s", want, text)
		}
	}
}

func TestControlPlaneOnlyCLIReviewIsVisible(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	upstreamAuthority, err := httpsproxy.EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("create upstream test authority: %v", err)
	}
	upstreamProxy, err := httpsproxy.Start(upstreamAuthority, httpsproxy.Options{
		Transport: cliRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.Hostname() != "api.anthropic.com" ||
				request.URL.Path != "/v1/messages" {
				t.Errorf("upstream request = %s %s", request.Method, request.URL)
			}
			const body = "model-ok\n"
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader(body)),
				ContentLength: int64(len(body)),
			}, nil
		}),
		AgentControlPlane: func(*http.Request) bool { return true },
	})
	if err != nil {
		t.Fatalf("start upstream test proxy: %v", err)
	}
	t.Cleanup(func() { _ = upstreamProxy.Close() })

	for _, key := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://"+upstreamProxy.Address())
	t.Setenv("https_proxy", "http://"+upstreamProxy.Address())
	t.Setenv("SSL_CERT_FILE", upstreamAuthority.CertificatePath)

	fakeDirectory := t.TempDir()
	fakeClaude := filepath.Join(fakeDirectory, "claude")
	if err := os.WriteFile(fakeClaude, []byte(
		"#!/bin/sh\nexec \""+curl+"\" -fsS --request POST --data '{}' "+
			"https://api.anthropic.com/v1/messages\n",
	), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	binary := buildTestBinary(t)

	for _, test := range []struct {
		name       string
		configured bool
	}{
		{name: "configured database", configured: true},
		{name: "no database", configured: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("UNRING_STATE_DIR", stateDir)
			var backendDone <-chan error
			if test.configured {
				connectionString, done := startReviewTestBackend(t, false)
				t.Setenv("DATABASE_URL", connectionString)
				backendDone = done
			} else {
				t.Setenv("DATABASE_URL", "")
			}

			command := exec.Command(binary, "run", "--outbound", "--watch-only", t.TempDir(), "--", fakeClaude)
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("control-plane-only CLI run: %v\n%s", err, output)
			}
			text := string(output)
			for _, want := range []string{
				"model-ok",
				"AGENT CONTROL PLANE — FORWARDED WITHOUT GATING",
				"[HTTP 200] POST https://api.anthropic.com:443/v1/messages",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("control-plane-only CLI output missing %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, quietSessionDisclosure) {
				t.Fatalf("intercepted control-plane traffic was called un-intercepted:\n%s", text)
			}
			for _, unwanted := range []string{
				"One decision applies", "Commit or discard?", "Up/down:",
			} {
				if strings.Contains(text, unwanted) {
					t.Fatalf("control-plane-only traffic manufactured %q:\n%s", unwanted, text)
				}
			}
			if !strings.Contains(text,
				"No commit/discard decision was needed; only observed HTTPS traffic is shown.") {
				t.Fatalf("observational review did not explain the missing decision prompt:\n%s", text)
			}
			if test.configured && strings.Contains(text, "NOT INTERCEPTED — no database traffic") {
				t.Fatalf("configured database was presented as unconfigured:\n%s", text)
			}
			if !test.configured && !strings.Contains(text,
				"NOT INTERCEPTED — no database traffic was intercepted") {
				t.Fatalf("no-database coverage disclosure missing:\n%s", text)
			}
			if backendDone != nil {
				if err := <-backendDone; err != nil {
					t.Fatalf("fake Postgres backend: %v", err)
				}
			}

			logCommand := exec.Command(binary, "log", "--json")
			logCommand.Env = os.Environ()
			logOutput, err := logCommand.CombinedOutput()
			if err != nil {
				t.Fatalf("unring log --json: %v\n%s", err, logOutput)
			}
			for _, want := range []string{
				`"url": "https://api.anthropic.com:443/v1/messages"`,
				`"disposition": "agent-control-plane"`,
			} {
				if !strings.Contains(string(logOutput), want) {
					t.Fatalf("control-plane JSON audit missing %q:\n%s", want, logOutput)
				}
			}

			var records []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(logOutput, &records); err != nil {
				t.Fatalf("decode unring log --json: %v\n%s", err, logOutput)
			}
			if len(records) != 1 || records[0].ID == "" {
				t.Fatalf("control-plane audit records = %#v, want one identified record", records)
			}
			auditCommand := exec.Command(binary, "log", records[0].ID)
			auditCommand.Env = os.Environ()
			auditOutput, err := auditCommand.CombinedOutput()
			if err != nil {
				t.Fatalf("unring log %s: %v\n%s", records[0].ID, err, auditOutput)
			}
			auditText := string(auditOutput)
			for _, want := range []string{
				"No commit/discard decision was needed; only observed HTTPS traffic is shown.",
				"AGENT CONTROL PLANE — FORWARDED WITHOUT GATING",
			} {
				if !strings.Contains(auditText, want) {
					t.Fatalf("control-plane audit replay missing %q:\n%s", want, auditText)
				}
			}
			if strings.Contains(auditText, "One decision applies") {
				t.Fatalf("control-plane audit replay manufactured a decision:\n%s", auditText)
			}
		})
	}
}

type cliRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function cliRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestGitPushOnlyRunGetsStructuralBlindSpotDisclosure(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", t.TempDir())
	binary := buildTestBinary(t)
	fakeDirectory := t.TempDir()
	fakeGit := filepath.Join(fakeDirectory, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	command := exec.Command(binary, "run", "--watch-only", t.TempDir(), "--", "git", "push")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git push-only run failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE",
		"git push over SSH",
		"direct-to-IP and raw-socket connections",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("git push-only run missing %q:\n%s", want, text)
		}
	}
}

func TestConfiguredQuietSessionPrintsOnlyDisclosure(t *testing.T) {
	connectionString, backendDone := startReviewTestBackend(t, false)
	t.Setenv("DATABASE_URL", connectionString)
	t.Setenv("UNRING_STATE_DIR", t.TempDir())

	binary := buildTestBinary(t)
	fakeDirectory := t.TempDir()
	fakeGit := filepath.Join(fakeDirectory, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", fakeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := exec.Command(binary, "run", "--discard", "--watch-only", t.TempDir(), "--", "git", "push")
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("configured quiet run failed: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("configured quiet run wrote stdout: %q", got)
	}
	if got, want := stderr.String(), quietSessionDisclosure+"\n"; got != want {
		t.Fatalf("configured quiet stderr = %q, want %q", got, want)
	}
	for _, unwanted := range []string{"UNRING SESSION REVIEW", "Commit or discard?", "Up/down:"} {
		if strings.Contains(stderr.String(), unwanted) {
			t.Fatalf("configured quiet disclosure included %q: %q", unwanted, stderr.String())
		}
	}
	if err := <-backendDone; err != nil {
		t.Fatalf("fake Postgres backend: %v", err)
	}
}

func TestDatabaseFreeStartupFailureRemainsNotStartedInAudit(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("UNRING_ADAPTERS", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	binary := buildTestBinary(t)
	command := exec.Command(binary, "run", "--outbound", "--watch-only", t.TempDir(), "--", "/bin/echo", "must-not-run")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != internalErrorExitCode {
		t.Fatalf("startup failure exit = %v, want %d\n%s", err, internalErrorExitCode, output)
	}
	if strings.Contains(string(output), "must-not-run\n") {
		t.Fatalf("startup failure launched child:\n%s", output)
	}

	logCommand := exec.Command(binary, "log", "--json")
	logCommand.Env = os.Environ()
	logOutput, err := logCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("unring log --json failed: %v\n%s", err, logOutput)
	}
	logText := string(logOutput)
	if !strings.Contains(logText, `"outcome": "not_started"`) ||
		strings.Contains(logText, `"outcome": "discarded"`) {
		t.Fatalf("pre-child database-free failure has false audit outcome:\n%s", logText)
	}
}

func TestBuiltBinaryHelpLeadsWithBoundedTaskWorkflow(t *testing.T) {
	binary := buildTestBinary(t)
	command := exec.Command(binary, "--help")
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("unring --help failed: %v\n%s", err, output)
	}
	text := string(output)
	primary := strings.Index(text, "Primary workflow (one bounded agent task that exits)")
	interactiveAlias := strings.Index(text, "unring claude|codex|opencode")
	if primary < 0 || interactiveAlias < 0 || primary > interactiveAlias {
		t.Fatalf("help did not present bounded work before interactive aliases:\n%s", text)
	}
	for _, want := range []string{
		"the shared transaction remains open for the",
		"whole child lifetime, holding locks and delaying cleanup",
		"DATABASE_URL may be unset",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("help missing %q:\n%s", want, text)
		}
	}
}
