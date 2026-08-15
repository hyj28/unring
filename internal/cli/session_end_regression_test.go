package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

type cancelableBatchPlatform struct {
	cliBackstopPlatform
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (platform *cancelableBatchPlatform) IsExcludedBatch(ctx context.Context, _ []string) ([]bool, error) {
	platform.once.Do(func() { close(platform.started) })
	<-ctx.Done()
	if platform.canceled != nil {
		close(platform.canceled)
	}
	if platform.release != nil {
		<-platform.release
	}
	return nil, ctx.Err()
}

func TestPostChildSignalsCancelSealAndRecordDiscard(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
			t.Setenv("DATABASE_URL", "")
			stateDir := t.TempDir()
			home := t.TempDir()
			project := t.TempDir()
			t.Setenv("UNRING_STATE_DIR", stateDir)
			t.Setenv("HOME", home)
			t.Chdir(project)
			outside := filepath.Join(home, "post-child-change.txt")
			platform := &cancelableBatchPlatform{
				cliBackstopPlatform: cliBackstopPlatform{excluded: map[string]bool{}},
				started:             make(chan struct{}),
			}
			restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
			defer restorePlatform()

			var stdout, stderr strings.Builder
			exited := make(chan int, 1)
			go func() {
				exited <- Main([]string{
					"run", "--watch", project, "--", "/bin/sh", "-c", `printf changed > "$1"`, "sh", outside,
				}, strings.NewReader(""), &stdout, &stderr)
			}()

			select {
			case <-platform.started:
			case <-time.After(10 * time.Second):
				t.Fatalf("post-child Time Machine batch did not start\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
			if err := syscall.Kill(os.Getpid(), signal); err != nil {
				t.Fatalf("send %s: %v", signal, err)
			}
			select {
			case code := <-exited:
				if code != 128+int(signal) {
					t.Fatalf("exit = %d, want literal %d\nstdout:\n%s\nstderr:\n%s", code, 128+int(signal), stdout.String(), stderr.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("run did not exit within five seconds after %s\nstdout:\n%s\nstderr:\n%s", signal, stdout.String(), stderr.String())
			}

			store, err := audit.OpenStoreAt(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			records, err := store.List()
			if err != nil || len(records) != 1 {
				t.Fatalf("records = %#v, %v, want literal one", records, err)
			}
			record := records[0]
			if record.Outcome != "discarded" || record.EndedAt.IsZero() {
				t.Fatalf("interrupted record = %#v, want ended discarded session", record)
			}
			if _, err := localrollback.LoadSealedSummary(stateDir, record.ID); err != nil {
				t.Fatalf("interrupted manifest was not sealed: %v", err)
			}
			if !record.Files.Interrupted {
				t.Fatalf("interrupted audit summary did not retain the interruption fact: %#v", record.Files)
			}
			var restoreOut, restoreErr strings.Builder
			if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &restoreOut, &restoreErr); code != 0 {
				t.Fatalf("restore listing exit = %d, want literal 0: %s", code, restoreErr.String())
			}
			if strings.Contains(restoreErr.String(), "capture is still in progress") {
				t.Fatalf("restore treated sealed interrupted manifest as pending: %s", restoreErr.String())
			}
			for _, literal := range []string{"INCOMPLETE", "scan was interrupted", "must not be treated as clean"} {
				if !strings.Contains(restoreOut.String(), literal) {
					t.Fatalf("restore listing omitted %q after interruption:\n%s", literal, restoreOut.String())
				}
			}
			if strings.Contains(restoreOut.String(), "changed no watched files") {
				t.Fatalf("interrupted restore listing falsely rendered clean:\n%s", restoreOut.String())
			}
			for _, humanOutput := range []string{stdout.String(), stderr.String(), restoreOut.String()} {
				if strings.Contains(humanOutput, "context canceled") || strings.Contains(humanOutput, "interrupted seal") {
					t.Fatalf("human output leaked internal cancellation vocabulary:\n%s", humanOutput)
				}
			}
			if !strings.Contains(stderr.String(), "stopping the scan and discarding the session") {
				t.Fatalf("interrupt output omitted post-child disposition:\n%s", stderr.String())
			}
			if signal == syscall.SIGTERM && (!strings.Contains(stderr.String(), "termination signal received") || strings.Contains(stderr.String(), "terminated received")) {
				t.Fatalf("SIGTERM acknowledgement is not grammatical:\n%s", stderr.String())
			}
			var logOut, logErr strings.Builder
			if code := Main([]string{"log"}, strings.NewReader(""), &logOut, &logErr); code != 0 {
				t.Fatalf("log exit = %d: %s", code, logErr.String())
			}
			if line := outputLineContaining(logOut.String(), record.ID); !strings.Contains(line, "interrupted") {
				t.Fatalf("abnormal session display = %q, want interrupted", line)
			}
			var jsonOut, jsonErr strings.Builder
			if code := Main([]string{"log", "--json", record.ID}, strings.NewReader(""), &jsonOut, &jsonErr); code != 0 {
				t.Fatalf("JSON log exit = %d: %s", code, jsonErr.String())
			}
			if !strings.Contains(jsonOut.String(), "context canceled") {
				t.Fatalf("structured record omitted precise cancellation cause:\n%s", jsonOut.String())
			}
		})
	}
}

func TestSecondPostChildSignalKeepsDurableDiscardInProgress(t *testing.T) {
	t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
	t.Setenv("DATABASE_URL", "")
	stateDir := t.TempDir()
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("UNRING_STATE_DIR", stateDir)
	t.Setenv("HOME", home)
	t.Chdir(project)
	platform := &cancelableBatchPlatform{
		cliBackstopPlatform: cliBackstopPlatform{excluded: map[string]bool{}},
		started:             make(chan struct{}),
		canceled:            make(chan struct{}),
		release:             make(chan struct{}),
	}
	restorePlatform := localrollback.SetVolumeSnapshotPlatformForTest(platform)
	defer restorePlatform()
	outside := filepath.Join(home, "second-signal.txt")
	var stdout, stderr strings.Builder
	exited := make(chan int, 1)
	go func() {
		exited <- Main([]string{
			"run", "--watch", project, "--", "/bin/sh", "-c", `printf changed > "$1"`, "sh", outside,
		}, strings.NewReader(""), &stdout, &stderr)
	}()
	select {
	case <-platform.started:
	case <-time.After(10 * time.Second):
		t.Fatal("post-child batch did not start")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-platform.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("first signal did not cancel the batch")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(platform.release)
	select {
	case code := <-exited:
		if code != 128+int(syscall.SIGINT) {
			t.Fatalf("exit = %d, want first-signal literal %d", code, 128+int(syscall.SIGINT))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish durable discard after second signal")
	}
	if !strings.Contains(stderr.String(), "second signal received (termination signal); safe discard finalization is already in progress and will not be skipped") {
		t.Fatalf("second-signal policy was not explained:\n%s", stderr.String())
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].Outcome != "discarded" || records[0].EndedAt.IsZero() {
		t.Fatalf("second-signal record = %#v, %v, want ended discard", records, err)
	}
}

func TestAutomaticRetentionAnnouncementsAreSingleAndBounded(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var oldIDs []string
	for index := 0; index < 52; index++ {
		record, err := audit.NewRecord(
			[]string{"expired-" + strconv.Itoa(index)},
			time.Date(2025, 1, 1, 0, index, 0, 0, time.UTC),
		)
		if err != nil {
			t.Fatal(err)
		}
		record.Outcome = "discarded"
		oldIDs = append(oldIDs, record.ID)
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), []byte("retention_days: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runMainSession(t, t.TempDir(), "/usr/bin/true")
	combined := stdout + stderr
	if got := strings.Count(combined, oldIDs[0]); got != 1 {
		t.Fatalf("oldest session id occurrences = %d, want literal 1\n%s", got, combined)
	}
	for index, id := range oldIDs {
		want := 1
		if index >= 50 {
			want = 0
		}
		if got := strings.Count(combined, id); got != want {
			t.Fatalf("session %d id occurrences = %d, want literal %d\n%s", index, got, want, combined)
		}
	}
	for _, literal := range []string{
		"automatic retention removed 52 sessions",
		"showing 50 of 52 automatic retention removals; 2 withheld",
		"Run unring log ",
		"past the configured age; removed stored session audit record",
		"shown-removal accounting",
	} {
		if !strings.Contains(combined, literal) {
			t.Fatalf("automatic retention output missing %q:\n%s", literal, combined)
		}
	}
	if got := strings.Count(combined, "copy-on-write clone references"); got != 1 {
		t.Fatalf("retention accounting caveat occurrences = %d, want literal 1\n%s", got, combined)
	}
	if got := strings.Count(stderr, "\n"); got > 25 {
		t.Fatalf("automatic retention stderr lines = %d, want at most literal 25\n%s", got, stderr)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || len(records[0].Files.RetentionEvents) != 52 {
		t.Fatalf("recorded retention events = %#v, %v, want independent literal 52", records, err)
	}
}

func TestRunAndLogBoundEachChangeGroupWithoutChangingManifest(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cursorDir := filepath.Join(home, ".cursor", "extensions")
	projectDir := filepath.Join(home, "project")
	for _, directory := range []string{cursorDir, projectDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `i=0; while [ "$i" -lt 55 ]; do printf x > "$1/cursor-$i"; printf y > "$2/regular-$i"; i=$((i+1)); done`
	stdout, stderr := runMainSession(t, home, "/bin/sh", "-c", script, "sh", cursorDir, projectDir)
	combined := stdout + stderr
	assertBoundedChangeRendering(t, combined)

	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want literal one", records, err)
	}
	record := records[0]
	if len(record.Files.Changes) != 110 {
		t.Fatalf("audit manifest changes = %d, want independent literal 110", len(record.Files.Changes))
	}
	sealed, err := localrollback.LoadSealedSummary(stateDir, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed.Changes) != 110 {
		t.Fatalf("sealed manifest changes = %d, want independent literal 110", len(sealed.Changes))
	}

	var logOut, logErr strings.Builder
	if code := Main([]string{"log", record.ID}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("log exit = %d: %s", code, logErr.String())
	}
	assertBoundedChangeRendering(t, logOut.String())
	var listOut strings.Builder
	if code := Main([]string{"log"}, strings.NewReader(""), &listOut, &logErr); code != 0 {
		t.Fatalf("log list exit = %d: %s", code, logErr.String())
	}
	if line := outputLineContaining(listOut.String(), record.ID); !strings.Contains(line, "no decision") || strings.Contains(line, "discarded") {
		t.Fatalf("successful restorable file-only displayed outcome = %q, want no decision", line)
	}

	var restoreOut, restoreErr strings.Builder
	if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &restoreOut, &restoreErr); code != 0 {
		t.Fatalf("restore listing exit = %d: %s", code, restoreErr.String())
	}
	if got := countChangeRows(restoreOut.String(), "/cursor-"); got != 55 {
		t.Fatalf("restore cursor detail count = %d, want literal 55", got)
	}
	if got := countChangeRows(restoreOut.String(), "/regular-"); got != 55 {
		t.Fatalf("restore regular detail count = %d, want literal 55", got)
	}
}

func TestSuccessfulNoOpRunExplicitlyReportsZeroFilesAndNoDecision(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	stdout, stderr := runMainSession(t, t.TempDir(), "/usr/bin/true")
	combined := stdout + stderr
	wantZero := "Files changed: 0 created, 0 modified, 0 deleted. The post-session scan completed."
	if !strings.Contains(combined, wantZero) {
		t.Fatalf("no-op run omitted explicit zero-change result %q:\n%s", wantZero, combined)
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want literal one", records, err)
	}
	var logOut, logErr strings.Builder
	if code := Main([]string{"log"}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("log exit = %d: %s", code, logErr.String())
	}
	if line := outputLineContaining(logOut.String(), records[0].ID); !strings.Contains(line, "no decision") || strings.Contains(line, "discarded") {
		t.Fatalf("successful file-only displayed outcome = %q, want no decision", line)
	}
	logOut.Reset()
	if code := Main([]string{"log", records[0].ID}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("log detail exit = %d: %s", code, logErr.String())
	}
	if !strings.Contains(logOut.String(), wantZero) {
		t.Fatalf("stored no-op rendering omitted explicit zero-change result:\n%s", logOut.String())
	}
	if strings.Contains(logOut.String(), "NO WHOLE-VOLUME BACKSTOP: \n") {
		t.Fatalf("stored rendering printed an empty backstop reason:\n%s", logOut.String())
	}
}

func TestExplicitDiscardRemainsDistinctFromNoDecision(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	var stdout, stderr strings.Builder
	if code := Main([]string{
		"run", "--discard", "--snapshot-cap-bytes", strconv.FormatInt(1<<40, 10),
		"--watch-only", root, "--", "/usr/bin/true",
	}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("explicit discard exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v, want literal one", records, err)
	}
	var logOut, logErr strings.Builder
	if code := Main([]string{"log"}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("log exit = %d: %s", code, logErr.String())
	}
	if line := outputLineContaining(logOut.String(), records[0].ID); !strings.Contains(line, "discarded") || strings.Contains(line, "no decision") {
		t.Fatalf("explicit discard displayed outcome = %q, want discarded", line)
	}
}

func TestEachWatchedRootGetsItsOwnBoundedChangeGroup(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	base := t.TempDir()
	noisy := filepath.Join(base, "a-noisy")
	important := filepath.Join(base, "z-important")
	for _, root := range []string{noisy, important} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := `i=0; while [ "$i" -lt 3000 ]; do printf n > "$1/n$(printf '%04d' "$i").txt"; i=$((i+1)); done; i=0; while [ "$i" -lt 5 ]; do printf q > "$2/important-$i.txt"; i=$((i+1)); done`
	args := []string{
		"run", "--snapshot-cap-bytes", strconv.FormatInt(1<<40, 10),
		"--watch-only", noisy, "--watch-only", important,
		"--", "/bin/sh", "-c", script, "sh", noisy, important,
	}
	var stdout, stderr strings.Builder
	if code := Main(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("two-root run exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if got := countChangeRows(stdout.String(), "/a-noisy/n"); got != 50 {
		t.Fatalf("printed noisy-root changes = %d, want literal 50\n%s", got, stdout.String())
	}
	if got := countChangeRows(stdout.String(), "/important-"); got != 5 {
		t.Fatalf("printed important-root changes = %d, want literal 5\n%s", got, stdout.String())
	}
	for _, root := range []string{noisy, important} {
		if !strings.Contains(stdout.String(), "WATCHED ROOT CHANGES — "+root) {
			t.Fatalf("output omitted watched-root group %s:\n%s", root, stdout.String())
		}
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || len(records[0].Files.Changes) != 3005 {
		t.Fatalf("recorded changes = %#v, %v, want independent literal 3005", records, err)
	}
	var logOut, logErr strings.Builder
	if code := Main([]string{"log", records[0].ID}, strings.NewReader(""), &logOut, &logErr); code != 0 {
		t.Fatalf("stored log exit = %d: %s", code, logErr.String())
	}
	if got := countChangeRows(logOut.String(), "/a-noisy/n"); got != 50 {
		t.Fatalf("stored noisy-root changes = %d, want literal 50", got)
	}
	if got := countChangeRows(logOut.String(), "/important-"); got != 5 {
		t.Fatalf("stored important-root changes = %d, want literal 5", got)
	}
}

func assertBoundedChangeRendering(t *testing.T, output string) {
	t.Helper()
	if got := countChangeRows(output, "/cursor-"); got != 50 {
		t.Fatalf("printed cursor changes = %d, want literal 50\n%s", got, output)
	}
	if got := countChangeRows(output, "/regular-"); got != 50 {
		t.Fatalf("printed regular changes = %d, want literal 50\n%s", got, output)
	}
	for _, literal := range []string{
		"Showing 50 of 55 agent own-state changes; 5 withheld",
		"Showing 50 of 55 changes under watched root",
		"Run unring restore ",
	} {
		if !strings.Contains(output, literal) {
			t.Fatalf("bounded change output missing %q:\n%s", literal, output)
		}
	}
}

func countChangeRows(output, pathFragment string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "  created  ") && strings.Contains(line, pathFragment) {
			count++
		}
	}
	return count
}

func TestCursorIsADeclaredAgentStateRoot(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "extensions", "literal-extension")
	if !localrollback.IsAgentStatePath(path, home) {
		t.Fatalf("%s was not recognized through the declared root list", path)
	}
	want := filepath.Join(home, ".cursor")
	if got := fmt.Sprint(localrollback.AgentStateRoots(home)); !strings.Contains(got, want) {
		t.Fatalf("declared roots %s do not contain literal %s", got, want)
	}
}

func TestBoundedRenderingDoesNotMutateCompleteChangeRecord(t *testing.T) {
	summary := localrollback.Summary{
		Watched: []string{"/literal/root"}, Retained: true,
		AgentStateRoots: []string{"/literal/root/.cursor"},
	}
	for index := 0; index < 120; index++ {
		directory := "/literal/root/project"
		if index >= 60 {
			directory = "/literal/root/.cursor/extensions"
		}
		summary.Changes = append(summary.Changes, localrollback.Change{
			Kind: "created", Path: filepath.Join(directory, fmt.Sprintf("item-%03d", index)),
			After: &localrollback.Entry{Type: "file"},
		})
	}
	before, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var live, stored strings.Builder
	printFileChanges(&live, "literal-session", summary, true)
	printAuditFiles(&stored, "literal-session", summary)
	after, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("complete change record changed during rendering\nbefore: %s\nafter:  %s", before, after)
	}
	if got := len(summary.Changes); got != 120 {
		t.Fatalf("complete change count = %d, want independent literal 120", got)
	}
}
