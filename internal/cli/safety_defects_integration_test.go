package cli

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentOwnStateIsGroupedSkippedByAllAndExplicitlyRestorable(t *testing.T) {
	stateDir := t.TempDir()
	home := t.TempDir()
	agentPath := filepath.Join(home, ".claude", "session-env", "session.json")
	userPath := filepath.Join(home, "Documents", "report.txt")
	for _, path := range []string{agentPath, userPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, "before")
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("HOME", home)
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)
	// buildTestBinary fabricates HOME to isolate default-scope tests; this case
	// deliberately exercises the declared roots beneath the caller's home.
	t.Setenv("HOME", home)

	run := exec.Command(binary, "run", "--discard", "--watch-only", home, "--",
		"/bin/sh", "-c", `printf after > "$1"; printf after > "$2"`, "unring-test", agentPath, userPath)
	run.Env = os.Environ()
	runOutput, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("agent-state run: %v\n%s", err, runOutput)
	}
	for _, want := range []string{"AGENT OWN-STATE CHANGES", agentPath, userPath} {
		if !strings.Contains(string(runOutput), want) {
			t.Fatalf("live listing omitted %q:\n%s", want, runOutput)
		}
	}

	sessionID := newestSessionID(t, binary)
	// The stored roots, not the restore process's environment, must control the
	// safety grouping. Model sudo/env -i by changing HOME before every read.
	t.Setenv("HOME", t.TempDir())
	log := exec.Command(binary, "log", sessionID)
	log.Env = os.Environ()
	logOutput, err := log.CombinedOutput()
	if err != nil {
		t.Fatalf("stored agent-state listing: %v\n%s", err, logOutput)
	}
	for _, want := range []string{"AGENT OWN-STATE CHANGES", agentPath, userPath} {
		if !strings.Contains(string(logOutput), want) {
			t.Fatalf("stored listing omitted %q:\n%s", want, logOutput)
		}
	}
	jsonLog := exec.Command(binary, "log", "--json", sessionID)
	jsonLog.Env = os.Environ()
	jsonOutput, err := jsonLog.CombinedOutput()
	if err != nil {
		t.Fatalf("stored agent-state roots: %v\n%s", err, jsonOutput)
	}
	if !strings.Contains(string(jsonOutput), `"agent_state_roots": [`) ||
		!strings.Contains(string(jsonOutput), filepath.Join(home, ".claude")) {
		t.Fatalf("audit record omitted persisted agent-state roots:\n%s", jsonOutput)
	}

	restoreAll := exec.Command(binary, "restore", "--all", sessionID)
	restoreAll.Env = os.Environ()
	restoreOutput, err := restoreAll.CombinedOutput()
	if err != nil {
		t.Fatalf("restore --all: %v\n%s", err, restoreOutput)
	}
	for _, want := range []string{
		"Skipped agent own-state paths", agentPath,
		"unring restore --all --include-agent-state " + sessionID,
	} {
		if !strings.Contains(string(restoreOutput), want) {
			t.Fatalf("restore --all skip disclosure omitted %q:\n%s", want, restoreOutput)
		}
	}
	assertTestFile(t, userPath, "before")
	assertTestFile(t, agentPath, "after")

	restoreSelected := exec.Command(binary, "restore", sessionID, agentPath)
	restoreSelected.Env = os.Environ()
	if output, err := restoreSelected.CombinedOutput(); err != nil {
		t.Fatalf("explicit agent-state restore: %v\n%s", err, output)
	}
	assertTestFile(t, agentPath, "before")
}

func TestUnsupportedFileTypeStaysRecordedAndMixedGapStaysLoud(t *testing.T) {
	stateDir := t.TempDir()
	watched, err := os.MkdirTemp("/tmp", "unring-socket-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(watched) })
	socketPath := filepath.Join(watched, "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	externalDirectory := t.TempDir()
	symlinkPath := filepath.Join(watched, "linked-directory")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	binary := buildTestBinary(t)

	run := exec.Command(binary, "run", "--discard", "--watch-only", watched, "--",
		"/bin/ln", "-s", externalDirectory, symlinkPath)
	run.Env = os.Environ()
	runOutput, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("unsupported-type run: %v\n%s", err, runOutput)
	}
	if !strings.Contains(string(runOutput), "UNSUPPORTED FILE TYPE (informational): "+socketPath) {
		t.Fatalf("live output omitted informational unsupported type:\n%s", runOutput)
	}
	if strings.Contains(string(runOutput), "FILE NOT SNAPSHOTTED: "+socketPath) {
		t.Fatalf("unsupported type retained actionable alarm prefix:\n%s", runOutput)
	}
	for _, want := range []string{
		"FILE COVERAGE INCOMPLETE", symlinkPath,
		"symlinked directory target is not followed or snapshotted",
	} {
		if !strings.Contains(string(runOutput), want) {
			t.Fatalf("mixed actionable gap omitted %q:\n%s", want, runOutput)
		}
	}

	sessionID := newestSessionID(t, binary)
	manifestPath := filepath.Join(stateDir, "snapshots", sessionID, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{socketPath, "unsupported file type"} {
		if !bytes.Contains(manifest, []byte(literal)) {
			t.Fatalf("stored manifest omitted literal %q:\n%s", literal, manifest)
		}
	}

	log := exec.Command(binary, "log", sessionID)
	log.Env = os.Environ()
	logOutput, err := log.CombinedOutput()
	if err != nil {
		t.Fatalf("stored unsupported-type listing: %v\n%s", err, logOutput)
	}
	if !strings.Contains(string(logOutput), "UNSUPPORTED FILE TYPE (informational): "+socketPath) {
		t.Fatalf("stored output drifted from live informational label:\n%s", logOutput)
	}
	if strings.Contains(string(logOutput), "FILE NOT SNAPSHOTTED: "+socketPath) {
		t.Fatalf("stored unsupported type retained actionable alarm prefix:\n%s", logOutput)
	}
}

func TestRetentionCapPreflightErrorsLeaveNoPendingAuditRecord(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	binary := buildTestBinary(t)
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "read stored cap", args: nil},
		{name: "save explicit cap", args: []string{"--snapshot-cap-bytes", "1024"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("UNRING_STATE_DIR", stateDir)
			if err := os.Mkdir(filepath.Join(stateDir, "snapshot-retention-cap"), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "child-ran")
			args := append([]string{"run"}, test.args...)
			args = append(args, "--watch-only", t.TempDir(), "--",
				"/bin/sh", "-c", `printf ran > "$1"`, "unring-test", marker)
			command := exec.Command(binary, args...)
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("retention-cap preflight unexpectedly succeeded:\n%s", output)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retention-cap error launched child: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(stateDir, "logs"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("retention-cap error left audit records: %#v", entries)
			}
		})
	}
}
