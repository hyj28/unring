package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/localrollback"
)

func TestRestoreCommandUsesRecordedBaselineWhenInterruptedChangeListIsEmpty(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	path := filepath.Join(root, "reports", "2024-summary.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("baseline,csv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := audit.NewRecord([]string{"literal-interrupted-command"}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{
		Watched: []string{root},
	}, 1<<30, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after interruption\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(root, "leaked-secrets.env")
	if err := os.WriteFile(created, []byte("session-created secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record.Files = session.SealContext(ctx, time.Unix(200, 0), nil)
	if len(record.Files.Changes) != 0 || record.Files.Complete || !record.Files.Interrupted {
		t.Fatalf("fixture summary = %#v, want independently verified interrupted empty list", record.Files)
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
	if code := Main([]string{"restore", record.ID, path}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("unforced conflicting baseline restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	for _, literal := range []string{path, "current contents differ from the recorded baseline", "later user work", "recorded baseline written alongside", "rerun with --force"} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("protected baseline restore omitted %q:\n%s", literal, stderr.String())
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "after interruption\n" {
		t.Fatalf("protected baseline restore contents = %q, %v; want independent current literal", contents, err)
	}
	sidecars, err := filepath.Glob(path + ".unring-*.snapshot")
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("protected baseline sidecars = %#v, %v; want independent literal one", sidecars, err)
	}
	sidecarContents, err := os.ReadFile(sidecars[0])
	if err != nil || string(sidecarContents) != "baseline,csv\n" {
		t.Fatalf("protected baseline sidecar = %q, %v; want independent baseline literal", sidecarContents, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", "--force", record.ID, path}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("forced named baseline restore exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, literal := range []string{"recorded baseline written back", path, "change list incomplete", "cannot confirm what the session did"} {
		if !strings.Contains(stdout.String(), literal) {
			t.Fatalf("forced named baseline restore omitted %q:\n%s", literal, stdout.String())
		}
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "baseline,csv\n" {
		t.Fatalf("forced named baseline restore contents = %q, %v; want independent literal", contents, err)
	}

	outside := filepath.Join(t.TempDir(), "not-covered.txt")
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", record.ID, outside}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("outside baseline restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	for _, literal := range []string{outside, "outside the recorded snapshot roots"} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("outside baseline restore omitted %q:\n%s", literal, stderr.String())
		}
	}

	if err := os.WriteFile(path, []byte("after before mixed batch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", "--force", record.ID, outside, path}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("mixed baseline batch exited zero despite one unavailable request\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unavailable "+outside) ||
		!strings.Contains(stderr.String(), "outside the recorded snapshot roots") {
		t.Fatalf("mixed baseline batch omitted unavailable path result:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "recorded baseline written back  "+path) {
		t.Fatalf("mixed baseline batch did not continue to the restorable path:\n%s", stdout.String())
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "baseline,csv\n" {
		t.Fatalf("mixed baseline batch contents = %q, %v; want independent baseline literal", contents, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", record.ID, created}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("unforced recorded-absence restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	for _, literal := range []string{created, "recorded baseline says this path did not exist", "means removing", "rerun with --force"} {
		if !strings.Contains(stderr.String(), literal) {
			t.Fatalf("recorded-absence refusal omitted %q:\n%s", literal, stderr.String())
		}
	}
	if contents, err := os.ReadFile(created); err != nil || string(contents) != "session-created secret\n" {
		t.Fatalf("unforced recorded-absence path = %q, %v; want independent retained literal", contents, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", "--force", record.ID, created}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("forced recorded-absence removal exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed   "+created) || strings.Contains(stdout.String(), "restored  "+created) {
		t.Fatalf("forced recorded-absence output did not report removal precisely:\n%s", stdout.String())
	}
	if _, err := os.Lstat(created); !os.IsNotExist(err) {
		t.Fatalf("forced recorded-absence path still exists: %v", err)
	}

	if err := os.WriteFile(path, []byte("must remain unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", "--all", record.ID}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("incomplete restore --all exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, literal := range []string{"restore --all", "refused", "Restoring every recorded baseline would revert paths the session may never have touched", "Named paths still work"} {
		if !strings.Contains(combined, literal) {
			t.Fatalf("incomplete restore --all omitted %q:\n%s", literal, combined)
		}
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "must remain unchanged\n" {
		t.Fatalf("refused restore --all contents = %q, %v; want independent unchanged literal", contents, err)
	}
}

func TestRestoreListingGenericIncompleteBannerExcludesOnlyRoutinePermissionFailures(t *testing.T) {
	tests := []struct {
		name        string
		category    string
		withChanges bool
		wantBanner  bool
	}{
		{name: "routine empty list", category: localrollback.CaptureFailureCategoryRoutinePermission},
		{name: "routine nonempty list", category: localrollback.CaptureFailureCategoryRoutinePermission, withChanges: true},
		{name: "non-routine empty list", wantBanner: true},
		{name: "non-routine nonempty list", withChanges: true, wantBanner: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("DATABASE_URL", "")
			t.Setenv("UNRING_STATE_DIR", stateDir)
			t.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "")
			record, err := audit.NewRecord([]string{"literal-banner-command"}, time.Unix(300, 0))
			if err != nil {
				t.Fatal(err)
			}
			failure := localrollback.CaptureFailure{
				Path: filepath.Join(stateDir, ".Trash"), Error: "operation not permitted", Category: test.category,
			}
			record.Files = localrollback.Summary{
				Complete: false, ScanFailures: []localrollback.CaptureFailure{failure},
				PostSessionFailures: []localrollback.CaptureFailure{failure},
				Error:               "post-session coverage incomplete: " + failure.Path + ": operation not permitted",
			}
			if test.withChanges {
				record.Files.Changes = []localrollback.Change{{Kind: "modified", Path: filepath.Join(stateDir, "watched.txt")}}
			}
			store, err := audit.OpenStoreAt(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr strings.Builder
			if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("restore listing exit = %d: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "CHANGE-LIST SCAN INCOMPLETE at recorded paths: "+failure.Path) {
				t.Fatalf("restore listing omitted precise scan path:\n%s", stdout.String())
			}
			gotBanner := strings.Contains(stdout.String(), "post-session scan did not produce a complete change list")
			if gotBanner != test.wantBanner {
				t.Fatalf("generic banner present = %t, want literal %t:\n%s", gotBanner, test.wantBanner, stdout.String())
			}
			if test.category == localrollback.CaptureFailureCategoryRoutinePermission && !test.withChanges {
				stdout.Reset()
				stderr.Reset()
				if code := Main([]string{"restore", "--all", record.ID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
					t.Fatalf("routine-only empty restore --all exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stdout.String(), "changed no watched files") ||
					!strings.Contains(stdout.String(), "CHANGE-LIST SCAN INCOMPLETE at recorded paths: "+failure.Path) ||
					strings.Contains(stdout.String()+stderr.String(), "restore --all "+record.ID+" refused") {
					t.Fatalf("routine-only empty restore --all was presented inconsistently:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestCompleteRestoreBatchReportsEverySelectionAndContinues(t *testing.T) {
	stateDir := t.TempDir()
	configureStorageHygieneTest(t, stateDir)
	root := t.TempDir()
	good := filepath.Join(root, "good.txt")
	typo := filepath.Join(root, "typo.txt")
	if err := os.WriteFile(good, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := audit.NewRecord([]string{"literal-complete-command"}, time.Unix(400, 0))
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := localrollback.StartScope(stateDir, record.ID, localrollback.Scope{
		Watched: []string{root},
	}, 1<<30, time.Unix(400, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record.Files = session.Seal(time.Unix(500, 0))
	if !record.Files.Complete || len(record.Files.Changes) != 1 || record.Files.Changes[0].Path != good {
		t.Fatalf("complete fixture summary = %#v, want independent one-change complete list", record.Files)
	}
	record.EndedAt = time.Unix(500, 0)
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := Main([]string{"restore", record.ID, typo, good}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("mixed complete restore exited zero despite typo\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unavailable "+typo) ||
		!strings.Contains(stderr.String(), "is not a changed path in this session") {
		t.Fatalf("mixed complete restore omitted typo result:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "restored  "+good) {
		t.Fatalf("mixed complete restore did not continue to good path:\n%s", stdout.String())
	}
	contents, err := os.ReadFile(good)
	if err != nil || string(contents) != "before\n" {
		t.Fatalf("mixed complete restore contents = %q, %v; want independent baseline literal", contents, err)
	}
}

func TestRestoreListingKeepsNoisyAgentStateExpanded(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	root := filepath.Join(t.TempDir(), ".codex")
	record, err := audit.NewRecord([]string{"literal-list-command"}, time.Unix(600, 0))
	if err != nil {
		t.Fatal(err)
	}
	record.Files = localrollback.Summary{
		AgentStateRoots: []string{root}, Complete: true, Retained: false,
	}
	for index := 0; index < 12; index++ {
		record.Files.Changes = append(record.Files.Changes, localrollback.Change{
			Kind: "modified", Path: filepath.Join(root, fmt.Sprintf("state-%02d.db", index)),
			RestoreSource: localrollback.RestoreSourceVolume,
		})
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := Main([]string{"restore", record.ID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("explicit restore listing exit = %d: %s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "  modified "+root+string(os.PathSeparator)+"state-"); got != 12 {
		t.Fatalf("explicit restore listing agent rows = %d, want independent literal 12:\n%s", got, stdout.String())
	}
	if strings.Contains(stdout.String(), "12 changes under agent-state root") {
		t.Fatalf("explicit restore listing incorrectly used summary collapse:\n%s", stdout.String())
	}
}

func TestAgentStateCollapseThresholdBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	for _, test := range []struct {
		count         int
		wantRows      int
		wantCountLine bool
	}{
		{count: 10, wantRows: 10},
		{count: 11, wantRows: 0, wantCountLine: true},
	} {
		t.Run(fmt.Sprintf("%d", test.count), func(t *testing.T) {
			summary := localrollback.Summary{
				Watched: []string{filepath.Dir(root)}, AgentStateRoots: []string{root},
				Complete: true, Retained: true,
			}
			for index := 0; index < test.count; index++ {
				summary.Changes = append(summary.Changes, localrollback.Change{
					Kind: "created", Path: filepath.Join(root, fmt.Sprintf("threshold-%02d", index)),
					RestoreSource: localrollback.RestoreSourceVolume,
				})
			}
			var output strings.Builder
			printFileChanges(&output, "threshold-session", summary, true)
			if got := countChangeRows(output.String(), "/threshold-"); got != test.wantRows {
				t.Fatalf("threshold %d detail rows = %d, want independent literal %d:\n%s", test.count, got, test.wantRows, output.String())
			}
			countLine := fmt.Sprintf("%d changes under agent-state root %s", test.count, root)
			if got := strings.Contains(output.String(), countLine); got != test.wantCountLine {
				t.Fatalf("threshold %d count line present = %t, want literal %t:\n%s", test.count, got, test.wantCountLine, output.String())
			}
		})
	}
}

func TestRestoreCoverageDoesNotPromiseUnavailableOrAllBaselineFallback(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("UNRING_STATE_DIR", stateDir)
	record, err := audit.NewRecord([]string{"literal-evicted-command"}, time.Unix(700, 0))
	if err != nil {
		t.Fatal(err)
	}
	record.Files = localrollback.Summary{
		Complete: false, Interrupted: true, InterruptedPhase: "post-session filesystem scan",
		Retained: false,
	}
	store, err := audit.OpenStoreAt(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	assertNoPromise := func(t *testing.T, output string) {
		t.Helper()
		for _, falsePromise := range []string{"can recover that baseline", "writes that baseline back"} {
			if strings.Contains(output, falsePromise) {
				t.Fatalf("restore disclosure made false baseline promise %q:\n%s", falsePromise, output)
			}
		}
	}
	var stdout, stderr strings.Builder
	path := filepath.Join(t.TempDir(), "evicted.txt")
	if code := Main([]string{"restore", record.ID, path}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("evicted named baseline restore exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	assertNoPromise(t, stdout.String()+stderr.String())
	if !strings.Contains(stderr.String(), "recorded baseline clone data is not retained") {
		t.Fatalf("evicted named baseline restore omitted unavailability:\n%s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"restore", "--all", record.ID}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("empty incomplete restore --all exited zero\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	assertNoPromise(t, stdout.String()+stderr.String())
}

func TestEndSummaryCollapsesNoisyAgentStateWithoutHidingExceptions(t *testing.T) {
	base := t.TempDir()
	agentRootOne := filepath.Join(base, ".codex")
	agentRootTwo := filepath.Join(base, ".cursor")
	watchedRoot := filepath.Join(base, "project")
	changes := []localrollback.Change{
		{Kind: "deleted", Path: filepath.Join(watchedRoot, "one.txt"), RestoreSource: localrollback.RestoreSourceClone},
		{Kind: "deleted", Path: filepath.Join(watchedRoot, "two.txt"), RestoreSource: localrollback.RestoreSourceClone},
		{Kind: "deleted", Path: filepath.Join(watchedRoot, "three.txt"), RestoreSource: localrollback.RestoreSourceClone},
	}
	for index := 0; index < 60; index++ {
		changes = append(changes, localrollback.Change{
			Kind: "modified", Path: filepath.Join(agentRootOne, fmt.Sprintf("cache-%03d.json", index)),
			RestoreSource: localrollback.RestoreSourceClone,
		})
	}
	for index := 0; index < 45; index++ {
		change := localrollback.Change{
			Kind: "modified", Path: filepath.Join(agentRootTwo, fmt.Sprintf("state-%03d.db", index)),
			RestoreSource: localrollback.RestoreSourceClone,
		}
		if index == 17 {
			change.RestoreSource = localrollback.RestoreSourceNone
			change.UnrestorableReason = "literal agent-state coverage failure"
		}
		changes = append(changes, change)
	}
	summary := localrollback.Summary{
		Watched: []string{watchedRoot}, AgentStateRoots: []string{agentRootOne, agentRootTwo},
		Changes: changes, Complete: false, Retained: true,
		Unscanned: []localrollback.CaptureFailure{{Path: agentRootOne, Error: "literal root scan failure"}},
	}

	assertCollapsed := func(t *testing.T, output string) {
		t.Helper()
		for _, literal := range []string{
			filepath.Join(watchedRoot, "one.txt"), filepath.Join(watchedRoot, "two.txt"), filepath.Join(watchedRoot, "three.txt"),
			"60 changes under agent-state root " + agentRootOne,
			"45 changes under agent-state root " + agentRootTwo,
			"List every recorded change with: unring restore literal-session-id",
			filepath.Join(agentRootTwo, "state-017.db"), "literal agent-state coverage failure",
			"WATCHED ROOT CHANGE LIST UNAVAILABLE: " + agentRootOne,
		} {
			if !strings.Contains(output, literal) {
				t.Fatalf("collapsed summary omitted %q:\n%s", literal, output)
			}
		}
		for _, hiddenRoutinePath := range []string{
			filepath.Join(agentRootOne, "cache-000.json"),
			filepath.Join(agentRootTwo, "state-044.db"),
		} {
			if strings.Contains(output, hiddenRoutinePath) {
				t.Fatalf("collapsed summary listed routine agent-state path %q:\n%s", hiddenRoutinePath, output)
			}
		}
	}

	var live strings.Builder
	printFileChanges(&live, "literal-session-id", summary, true)
	assertCollapsed(t, live.String())

	var stored strings.Builder
	printAuditFiles(&stored, "literal-session-id", summary)
	assertCollapsed(t, stored.String())
}

func TestEndSummaryCollapsesSnapshotOnlyAgentStateChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	changes := make([]localrollback.Change, 0, 105)
	for index := 0; index < 105; index++ {
		changes = append(changes, localrollback.Change{
			Kind: "modified", Path: filepath.Join(root, fmt.Sprintf("snapshot-only-%03d.db", index)),
			RestoreSource: localrollback.RestoreSourceVolume,
		})
	}
	summary := localrollback.Summary{
		Watched: []string{filepath.Dir(root)}, AgentStateRoots: []string{root},
		Changes: changes, Complete: true, Retained: true,
	}
	for name, render := range map[string]func(*strings.Builder){
		"live": func(output *strings.Builder) { printFileChanges(output, "snapshot-only-session", summary, true) },
		"stored": func(output *strings.Builder) {
			printAuditFiles(output, "snapshot-only-session", summary)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var output strings.Builder
			render(&output)
			for _, literal := range []string{
				"105 changes under agent-state root " + root,
				"List every recorded change with: unring restore snapshot-only-session",
			} {
				if !strings.Contains(output.String(), literal) {
					t.Fatalf("snapshot-only collapse omitted %q:\n%s", literal, output.String())
				}
			}
			if strings.Contains(output.String(), "snapshot-only-000.db") || strings.Contains(output.String(), "SNAPSHOT ONLY") {
				t.Fatalf("snapshot-only agent-state details escaped collapsed summary:\n%s", output.String())
			}
		})
	}
}
