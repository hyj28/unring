// Package localrollback snapshots watched files and restores session changes later.
package localrollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	// DefaultRetentionBytes is the default measured-allocation cap for retained snapshots.
	DefaultRetentionBytes int64 = 5 << 30
	// DefaultRetentionDays is the default maximum age for stored sessions.
	DefaultRetentionDays = 14
	manifestVersion      = 1
	RestoreSourceClone   = "clone"
	RestoreSourceVolume  = "volume-snapshot"
	RestoreSourceNone    = "none"
	// UnsupportedFileTypeCoverageReason is retained for special files such as
	// Unix sockets that cannot be restored meaningfully per path.
	UnsupportedFileTypeCoverageReason = "unsupported file type is outside snapshot coverage"
	// AutomaticContentComparisonBytes bounds end-of-session byte comparisons.
	// Larger files retain the metadata oracle's safe false-positive behavior.
	AutomaticContentComparisonBytes int64 = 8 << 20
)

// Entry is the metadata used to detect a file change and a later restore conflict.
type Entry struct {
	Size       int64  `json:"size"`
	MTime      int64  `json:"mtime_ns"`
	CTime      int64  `json:"ctime_ns"`
	Mode       uint32 `json:"mode"`
	Type       string `json:"type"`
	Links      uint64 `json:"links,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
}

// CaptureFailure identifies a path that was not protected by the snapshot.
type CaptureFailure struct {
	Path     string `json:"path"`
	Error    string `json:"error"`
	Category string `json:"category,omitempty"`
}

const CaptureFailureCategoryRoutinePermission = "routine_permission_refusal"

// IsUnsupportedFileTypeFailure identifies the declared informational coverage
// class for special files that cannot be restored meaningfully per path.
func IsUnsupportedFileTypeFailure(failure CaptureFailure) bool {
	return strings.HasPrefix(failure.Error, "unsupported file type")
}

// HasOnlyUnsupportedFileTypeFailures reports the one incomplete state that is
// informational rather than actionable. It is deliberately strict: any scan
// failure, unexplained error text, or additional persistence/storage error
// makes the incomplete summary actionable.
func HasOnlyUnsupportedFileTypeFailures(summary Summary) bool {
	if summary.Complete || len(summary.PostSessionFailures) == 0 || len(summary.ScanFailures) != 0 {
		return false
	}
	for _, failure := range summary.PostSessionFailures {
		if !IsUnsupportedFileTypeFailure(failure) {
			return false
		}
	}
	wantError := formatFailures("post-session coverage incomplete", summary.PostSessionFailures)
	return summary.Error == wantError
}

// HasOnlyRoutinePermissionScanFailures reports an incomplete widened scan whose
// only gaps are OS-classified permission refusals. The coverage remains
// incomplete and fully disclosed; this classification only prevents permanent
// background TCC restrictions from making an otherwise normal session outcome
// appear abnormal.
func HasOnlyRoutinePermissionScanFailures(summary Summary) bool {
	if summary.Complete || summary.Interrupted || len(summary.Unscanned) != 0 ||
		len(summary.ScanFailures) == 0 || len(summary.PostSessionFailures) == 0 {
		return false
	}
	for _, failure := range summary.Uncaptured {
		if !IsUnsupportedFileTypeFailure(failure) {
			return false
		}
	}
	scanFailures := make(map[string]CaptureFailure, len(summary.ScanFailures))
	for _, failure := range summary.ScanFailures {
		if failure.Category != CaptureFailureCategoryRoutinePermission {
			return false
		}
		scanFailures[failure.Path] = failure
	}
	if len(scanFailures) != len(summary.PostSessionFailures) {
		return false
	}
	for _, failure := range summary.PostSessionFailures {
		if recorded, ok := scanFailures[failure.Path]; !ok || recorded != failure {
			return false
		}
	}
	return summary.Error == formatFailures("post-session coverage incomplete", summary.PostSessionFailures)
}

// Change is one created, modified, or deleted path.
type Change struct {
	Kind               string          `json:"kind"`
	Path               string          `json:"path"`
	VolumeSnapshotPath string          `json:"volume_snapshot_path,omitempty"`
	Before             *Entry          `json:"before,omitempty"`
	After              *Entry          `json:"after,omitempty"`
	RestoreSource      string          `json:"restore_source,omitempty"`
	VolumeSnapshot     *VolumeSnapshot `json:"volume_snapshot,omitempty"`
	UnrestorableReason string          `json:"unrestorable_reason,omitempty"`
}

// RestoreRecord is a durable account of one requested restore.
type RestoreRecord struct {
	Path       string    `json:"path"`
	Status     string    `json:"status"`
	Sidecar    string    `json:"snapshot_sidecar,omitempty"`
	Error      string    `json:"error,omitempty"`
	RestoredAt time.Time `json:"restored_at"`
}

// RetentionEvent records one automatic retention action announced by a run.
type RetentionEvent struct {
	SessionID    string `json:"session_id"`
	StorageBytes int64  `json:"storage_bytes"`
	StorageExact bool   `json:"storage_bytes_exact"`
	Expired      bool   `json:"expired"`
	CapRequired  bool   `json:"cap_required"`
}

// Summary is stored in the session audit record.
type Summary struct {
	Watched             []string         `json:"watched_paths"`
	AgentStateRoots     []string         `json:"agent_state_roots,omitempty"`
	Uncaptured          []CaptureFailure `json:"uncaptured_paths"`
	Unscanned           []CaptureFailure `json:"unscanned_watched_roots,omitempty"`
	PostSessionFailures []CaptureFailure `json:"post_session_coverage_failures,omitempty"`
	Changes             []Change         `json:"changes"`
	Complete            bool             `json:"complete"`
	Interrupted         bool             `json:"post_session_scan_interrupted,omitempty"`
	InterruptedPhase    string           `json:"interrupted_phase,omitempty"`
	Error               string           `json:"error,omitempty"`
	Storage             string           `json:"storage"`
	LogicalBytes        int64            `json:"logical_bytes"`
	StorageBytes        int64            `json:"storage_bytes"`
	StorageExact        bool             `json:"storage_bytes_exact"`
	CopiedBytes         int64            `json:"copied_bytes"`
	RetentionCap        int64            `json:"retention_cap_bytes"`
	Retained            bool             `json:"retained"`
	Evicted             []string         `json:"evicted_sessions,omitempty"`
	RetentionEvents     []RetentionEvent `json:"retention_events,omitempty"`
	RestoreEvents       []RestoreRecord  `json:"restores,omitempty"`
	ChangeListScope     string           `json:"change_list_scope,omitempty"`
	ChangeListRoots     []string         `json:"change_list_roots,omitempty"`
	ScanRoot            string           `json:"change_scan_root,omitempty"`
	ScanExcluded        []string         `json:"change_scan_excluded,omitempty"`
	ScanFailures        []CaptureFailure `json:"change_scan_failures,omitempty"`
	ScanBeforeFiles     int              `json:"change_scan_before_files,omitempty"`
	ScanAfterFiles      int              `json:"change_scan_after_files,omitempty"`
	ScanBeforeMillis    int64            `json:"change_scan_before_ms,omitempty"`
	ScanAfterMillis     int64            `json:"change_scan_after_ms,omitempty"`
	Backstop            Backstop         `json:"backstop"`
	manifestEndedAt     time.Time
}

type rootManifest struct {
	Path             string            `json:"path"`
	Source           string            `json:"source"`
	Existed          bool              `json:"existed"`
	Snapshot         string            `json:"snapshot"`
	Before           map[string]Entry  `json:"before"`
	UncapturedBefore map[string]Entry  `json:"uncaptured_before,omitempty"`
	Uncaptured       map[string]string `json:"uncaptured,omitempty"`
}

type manifest struct {
	Version             int              `json:"version"`
	SessionID           string           `json:"session_id"`
	StartedAt           time.Time        `json:"started_at"`
	EndedAt             time.Time        `json:"ended_at,omitempty"`
	Roots               []rootManifest   `json:"roots"`
	AgentStateRoots     []string         `json:"agent_state_roots,omitempty"`
	Excluded            []string         `json:"excluded,omitempty"`
	After               map[string]Entry `json:"after,omitempty"`
	Changes             []Change         `json:"changes,omitempty"`
	Complete            bool             `json:"complete"`
	Interrupted         bool             `json:"post_session_scan_interrupted,omitempty"`
	InterruptedPhase    string           `json:"interrupted_phase,omitempty"`
	Error               string           `json:"error,omitempty"`
	Storage             string           `json:"storage"`
	LogicalBytes        int64            `json:"logical_bytes"`
	StorageBytes        int64            `json:"storage_bytes"`
	StorageExact        bool             `json:"storage_bytes_exact"`
	CopiedBytes         int64            `json:"copied_bytes"`
	RetentionCap        int64            `json:"retention_cap_bytes"`
	ChangeListScope     string           `json:"change_list_scope,omitempty"`
	ChangeListRoots     []string         `json:"change_list_roots,omitempty"`
	ScanRoot            string           `json:"scan_root,omitempty"`
	ScanExcluded        []string         `json:"scan_excluded,omitempty"`
	ScanExcludedNames   []string         `json:"scan_excluded_names,omitempty"`
	ScanFailures        []CaptureFailure `json:"scan_failures,omitempty"`
	PostSessionFailures []CaptureFailure `json:"post_session_coverage_failures,omitempty"`
	Unscanned           []CaptureFailure `json:"unscanned_watched_roots,omitempty"`
	ScanBeforeFiles     int              `json:"scan_before_files,omitempty"`
	ScanAfterFiles      int              `json:"scan_after_files,omitempty"`
	ScanBeforeMillis    int64            `json:"scan_before_ms,omitempty"`
	ScanAfterMillis     int64            `json:"scan_after_ms,omitempty"`
	Backstop            Backstop         `json:"backstop"`
}

// Session owns the in-progress snapshot for a wrapped child.
type Session struct {
	dir      string
	manifest manifest
	summary  Summary
	platform VolumeSnapshotPlatform
	// The widened baseline is needed only by this live process. Persisting it
	// would serialize and fsync hundreds of thousands of entries repeatedly.
	scanBefore map[string]Entry
}

// SealProgress reports bounded progress through the potentially long
// post-child Time Machine inclusion phase.
type SealProgress struct {
	Phase     string
	Completed int
	Total     int
}

// Usage describes retained snapshot storage.
type Usage struct {
	Bytes    int64
	CapBytes int64
	Sessions int
	Exact    bool
	Warnings []RetentionWarning
}

// StoredSession is the audit-layer identity and start time used by the shared
// age-and-space retention planner.
type StoredSession struct {
	ID           string
	StartedAt    time.Time
	StorageBytes int64
	StorageExact bool
	StorageKnown bool
}

// RetentionRemoval is one session selected once by the cooperative age and
// byte-cap policy. StorageBytes is unring's measured snapshot charge, not a
// promise that deleting copy-on-write references immediately frees that many
// filesystem bytes.
type RetentionRemoval struct {
	SessionID    string
	StartedAt    time.Time
	StorageBytes int64
	StorageExact bool
	HasSnapshot  bool
	Expired      bool
	CapRequired  bool
}

// RetentionWarning reports damaged snapshot state that could not contribute
// reliable byte accounting. Other healthy sessions remain eligible.
type RetentionWarning struct {
	SessionID string
	Error     string
}

// RetentionApplyError identifies a partially applied removal. When
// SnapshotRemoved is true, the clone data is already gone even though the
// audit-record finalization failed.
type RetentionApplyError struct {
	Removal         RetentionRemoval
	SnapshotRemoved bool
	Err             error
}

func (err *RetentionApplyError) Error() string { return err.Err.Error() }
func (err *RetentionApplyError) Unwrap() error { return err.Err }

// RetentionPlan is a stable oldest-first retention decision.
type RetentionPlan struct {
	Removals []RetentionRemoval
	Warnings []RetentionWarning
	Before   Usage
	After    Usage
}

// DefaultWatchPaths returns the deliberately narrow default scope: the project
// tree and the high-risk user directories named in the design.
func DefaultWatchPaths(workingDirectory, homeDirectory string) ([]string, error) {
	project, err := projectRoot(workingDirectory)
	if err != nil {
		return nil, err
	}
	paths := []string{project}
	if homeDirectory != "" {
		paths = append(paths,
			filepath.Join(homeDirectory, "Documents"),
			filepath.Join(homeDirectory, "Desktop"),
			filepath.Join(homeDirectory, ".config"),
			filepath.Join(homeDirectory, ".ssh"),
			filepath.Join(homeDirectory, ".aws"),
		)
	}
	return normalizeRoots(paths)
}

// RetentionCap reads the test/install override and otherwise returns the 5 GiB default.
func RetentionCap() (int64, error) {
	value := strings.TrimSpace(os.Getenv("UNRING_SNAPSHOT_CAP_BYTES"))
	if value == "" {
		return DefaultRetentionBytes, nil
	}
	capBytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil || capBytes < 0 {
		return 0, fmt.Errorf("UNRING_SNAPSHOT_CAP_BYTES must be a non-negative integer, got %q", value)
	}
	return capBytes, nil
}

// SaveRetentionCap persists an explicitly selected cap so `unring snapshots`
// and later sessions report and enforce the same unit and value.
func SaveRetentionCap(stateDir string, capBytes int64) error {
	if capBytes < 0 {
		return errors.New("snapshot retention cap cannot be negative")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, "snapshot-retention-cap")
	temporary, err := os.CreateTemp(stateDir, ".snapshot-retention-cap-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := fmt.Fprintf(temporary, "%d\n", capBytes); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// RetentionCapForState returns the last cap explicitly persisted for this
// state directory, then the environment default, then the built-in default.
// A persisted cap must win so inspection cannot report a value different from
// the value that the most recent explicit run enforced.
func RetentionCapForState(stateDir string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "snapshot-retention-cap"))
	if errors.Is(err, os.ErrNotExist) {
		if strings.TrimSpace(os.Getenv("UNRING_SNAPSHOT_CAP_BYTES")) != "" {
			return RetentionCap()
		}
		return DefaultRetentionBytes, nil
	}
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	capBytes, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || capBytes < 0 {
		return 0, fmt.Errorf("stored snapshot retention cap is invalid: %q", value)
	}
	return capBytes, nil
}

// Start captures watched paths before the child starts. It first attempts the
// platform's recursive fast path, then falls back to per-entry capture so that
// one unreadable path does not erase coverage for the rest of a tree.
func Start(stateDir, sessionID string, watched []string, capBytes int64, now time.Time) (*Session, Summary, error) {
	return start(context.Background(), stateDir, sessionID, watched, nil, nil, AgentStateRoots(""), ChangeListScopeCloneOnly, watched, "", nil, nil, "", capBytes, now)
}

// StartWithExclusions captures watched paths while omitting physically resolved
// config exclusions in addition to unring's own state directory.
func StartWithExclusions(stateDir, sessionID string, watched, excluded []string, capBytes int64, now time.Time) (*Session, Summary, error) {
	return start(context.Background(), stateDir, sessionID, watched, excluded, nil, AgentStateRoots(""), ChangeListScopeCloneOnly, watched, "", nil, nil, "", capBytes, now)
}

// StartScope captures a previously resolved scope, including any explicit
// watches that configuration exclusions made uncapturable.
func StartScope(stateDir, sessionID string, scope Scope, capBytes int64, now time.Time) (*Session, Summary, error) {
	return StartScopeContext(context.Background(), stateDir, sessionID, scope, capBytes, now)
}

// StartScopeContext captures the pre-session snapshot and baseline while
// allowing an interrupted run to stop before its child is launched.
func StartScopeContext(ctx context.Context, stateDir, sessionID string, scope Scope, capBytes int64, now time.Time) (*Session, Summary, error) {
	agentStateRoots := scope.AgentStateRoots
	if len(agentStateRoots) == 0 {
		agentStateRoots = AgentStateRoots("")
	}
	return start(
		ctx,
		stateDir, sessionID, scope.Watched, scope.Excluded, scope.Uncaptured,
		agentStateRoots,
		scope.ChangeListScope, scope.ChangeListRoots,
		scope.ScanRoot, scope.ScanExcluded, scope.ScanExcludedNames, scope.ScanError,
		capBytes, now,
	)
}

func start(
	ctx context.Context,
	stateDir, sessionID string,
	watched, excluded []string,
	preflightFailures []CaptureFailure,
	agentStateRoots []string,
	changeListScope string,
	changeListRoots []string,
	scanRoot string,
	scanExcluded, scanExcludedNames []string,
	scanError string,
	capBytes int64,
	now time.Time,
) (*Session, Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, Summary{}, err
	}
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return nil, Summary{}, errors.New("start file snapshot: invalid session id")
	}
	if len(agentStateRoots) == 0 {
		return nil, Summary{}, errors.New("start file snapshot: declared agent-state roots are unavailable")
	}
	roots, err := normalizeRoots(watched)
	if err != nil {
		return nil, Summary{}, fmt.Errorf("resolve watched paths: %w", err)
	}
	snapshotRoot := filepath.Join(stateDir, "snapshots")
	if err := os.MkdirAll(snapshotRoot, 0o700); err != nil {
		return nil, Summary{}, fmt.Errorf("create snapshot directory: %w", err)
	}
	if err := os.Chmod(snapshotRoot, 0o700); err != nil {
		return nil, Summary{}, fmt.Errorf("restrict snapshot directory: %w", err)
	}
	temporary, err := os.MkdirTemp(snapshotRoot, ".snapshot-*.tmp")
	if err != nil {
		return nil, Summary{}, fmt.Errorf("create temporary snapshot: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeSnapshotTree(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, Summary{}, fmt.Errorf("restrict temporary snapshot: %w", err)
	}

	m := manifest{
		Version: manifestVersion, SessionID: sessionID, StartedAt: now.UTC(),
		Complete: false, RetentionCap: capBytes, ScanRoot: scanRoot,
		AgentStateRoots:   append([]string(nil), agentStateRoots...),
		ChangeListScope:   changeListScope,
		ChangeListRoots:   append([]string(nil), changeListRoots...),
		ScanExcluded:      append([]string(nil), scanExcluded...),
		ScanExcludedNames: append([]string(nil), scanExcludedNames...),
	}
	platform := currentSnapshotPlatform()
	backstopPaths := append([]string(nil), roots...)
	if scanRoot != "" {
		backstopPaths = append(backstopPaths, scanRoot)
	}
	m.Backstop = takeBackstop(backstopPaths, platform)
	absoluteStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, Summary{}, fmt.Errorf("resolve state directory: %w", err)
	}
	resolvedStateDir, err := filepath.EvalSymlinks(absoluteStateDir)
	if err != nil {
		return nil, Summary{}, fmt.Errorf("resolve state directory symlinks: %w", err)
	}
	m.Excluded = append([]string(nil), excluded...)
	m.Excluded = append(m.Excluded, filepath.Clean(resolvedStateDir))
	m.Excluded = append(m.Excluded, overlappingRootExclusions(roots)...)
	m.ScanExcluded = append(m.ScanExcluded, filepath.Clean(resolvedStateDir))
	availableBefore, storageMeasured, storageMeasureErr := filesystemAvailableBytes(snapshotRoot)
	if storageMeasureErr != nil {
		return nil, Summary{}, fmt.Errorf("measure snapshot storage before capture: %w", storageMeasureErr)
	}
	var failures []CaptureFailure
	methods := make(map[string]bool)
	for index, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, Summary{}, err
		}
		relSnapshot := filepath.Join("roots", fmt.Sprintf("%06d", index))
		destination := filepath.Join(temporary, relSnapshot)
		rootState, rootFailures, rootMethods, logicalBytes, copiedBytes, captureErr := captureRootContext(ctx, root, destination, m.Excluded)
		rootState.Snapshot = relSnapshot
		m.Roots = append(m.Roots, rootState)
		failures = append(failures, rootFailures...)
		for method := range rootMethods {
			methods[method] = true
		}
		m.LogicalBytes += logicalBytes
		m.CopiedBytes += copiedBytes
		if captureErr != nil {
			return nil, Summary{}, fmt.Errorf("snapshot %s: %w", root, captureErr)
		}
	}
	for _, failure := range preflightFailures {
		if !attachManifestFailure(&m, failure) {
			return nil, Summary{}, fmt.Errorf("record preflight snapshot failure for %s: no watched root contains it", failure.Path)
		}
		failures = append(failures, failure)
	}
	var scanBefore map[string]Entry
	if scanError != "" {
		m.ScanFailures = append(m.ScanFailures, CaptureFailure{Path: "home directory", Error: scanError})
	} else if scanRoot != "" {
		scanStarted := time.Now()
		scanBefore, m.ScanFailures, err = scanRootWithNamesContext(ctx, scanRoot, m.ScanExcluded, m.ScanExcludedNames)
		m.ScanBeforeMillis = time.Since(scanStarted).Milliseconds()
		m.ScanBeforeFiles = len(scanBefore)
		if err != nil {
			if ctx.Err() != nil {
				return nil, Summary{}, ctx.Err()
			}
			m.ScanFailures = append(m.ScanFailures, CaptureFailure{Path: scanRoot, Error: err.Error()})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, Summary{}, err
	}
	failures = mergeFailures(failures)
	m.Storage = storageDescription(methods)
	if err := writeManifest(temporary, m); err != nil {
		return nil, Summary{}, err
	}
	finalDir := filepath.Join(snapshotRoot, sessionID)
	if err := os.Rename(temporary, finalDir); err != nil {
		return nil, Summary{}, fmt.Errorf("publish file snapshot: %w", err)
	}
	cleanup = false
	availableAfter, measuredAfter, storageMeasureErr := filesystemAvailableBytes(snapshotRoot)
	if storageMeasureErr != nil {
		_ = removeSnapshotTree(finalDir)
		return nil, Summary{}, fmt.Errorf("measure snapshot storage after capture: %w", storageMeasureErr)
	}
	m.StorageExact = storageMeasured && measuredAfter
	if m.StorageExact {
		m.StorageBytes = consumedBytes(availableBefore, availableAfter)
		if manifestBytes, manifestErr := allocatedPath(filepath.Join(finalDir, "manifest.json")); manifestErr != nil {
			_ = removeSnapshotTree(finalDir)
			return nil, Summary{}, fmt.Errorf("measure snapshot manifest allocation: %w", manifestErr)
		} else if manifestBytes > m.StorageBytes {
			m.StorageBytes = manifestBytes
		}
	} else {
		m.StorageBytes, err = allocatedTree(finalDir)
		if err != nil {
			_ = removeSnapshotTree(finalDir)
			return nil, Summary{}, fmt.Errorf("measure file snapshot storage upper bound: %w", err)
		}
	}
	if err := writeManifest(finalDir, m); err != nil {
		_ = removeSnapshotTree(finalDir)
		return nil, Summary{}, err
	}
	summary := Summary{
		Watched: roots, Uncaptured: failures, Complete: false,
		AgentStateRoots: append([]string(nil), m.AgentStateRoots...),
		Storage:         m.Storage, LogicalBytes: m.LogicalBytes, StorageBytes: m.StorageBytes,
		StorageExact: m.StorageExact, CopiedBytes: m.CopiedBytes,
		RetentionCap: capBytes, Retained: true,
		ChangeListScope: m.ChangeListScope,
		ChangeListRoots: append([]string(nil), m.ChangeListRoots...),
		ScanRoot:        m.ScanRoot, ScanExcluded: append([]string(nil), m.ScanExcluded...),
		ScanFailures:    append([]CaptureFailure(nil), m.ScanFailures...),
		ScanBeforeFiles: m.ScanBeforeFiles, ScanBeforeMillis: m.ScanBeforeMillis,
		Backstop: m.Backstop,
	}
	session := &Session{
		dir: finalDir, manifest: m, summary: summary, platform: platform,
		scanBefore: scanBefore,
	}
	session.summary = summary
	return session, summary, nil
}

func attachManifestFailure(value *manifest, failure CaptureFailure) bool {
	matched := -1
	for index := range value.Roots {
		root := &value.Roots[index]
		if failure.Path != root.Path && !strings.HasPrefix(failure.Path, root.Path+string(os.PathSeparator)) {
			continue
		}
		if matched < 0 || len(root.Path) > len(value.Roots[matched].Path) {
			matched = index
		}
	}
	if matched >= 0 {
		root := &value.Roots[matched]
		root.Uncaptured[failure.Path] = failure.Error
		if root.UncapturedBefore == nil {
			root.UncapturedBefore = make(map[string]Entry)
		}
		prefix := map[string]bool{failure.Path: true}
		for path, entry := range root.Before {
			if coveredBy(path, prefix) {
				root.UncapturedBefore[path] = entry
			}
		}
		return true
	}
	return false
}

// Seal scans the watched paths after the child exits and records the diff.
func (s *Session) Seal(now time.Time) Summary {
	return s.SealContext(context.Background(), now, nil)
}

// SealContext seals the durable manifest even when ctx interrupts the scan.
// The resulting incomplete summary is safe to record as a discarded session.
func (s *Session) SealContext(ctx context.Context, now time.Time, progress func(SealProgress)) Summary {
	return s.sealContext(ctx, now, progress, "post-session filesystem scan")
}

// SealInterruptedContext publishes a definite incomplete manifest when a run
// is interrupted after snapshot capture but before its child starts.
func (s *Session) SealInterruptedContext(ctx context.Context, now time.Time, phase string) Summary {
	return s.sealContext(ctx, now, nil, phase)
}

func (s *Session) sealContext(ctx context.Context, now time.Time, progress func(SealProgress), interruptedPhase string) Summary {
	availableBefore, measuredBefore, measureBeforeErr := filesystemAvailableBytes(s.dir)
	diffExcluded := make(map[string]bool)
	for _, root := range s.manifest.Roots {
		for path := range root.Uncaptured {
			diffExcluded[path] = true
		}
	}
	after := make(map[string]Entry)
	rootAfter := make([]map[string]Entry, len(s.manifest.Roots))
	rootScanned := make([]bool, len(s.manifest.Roots))
	rootDiffExcluded := make([]map[string]bool, len(s.manifest.Roots))
	s.manifest.Unscanned = nil
	initialUncaptured := make([]map[string]bool, len(s.manifest.Roots))
	for index, root := range s.manifest.Roots {
		initialUncaptured[index] = make(map[string]bool, len(root.Uncaptured))
		for path := range root.Uncaptured {
			initialUncaptured[index][path] = true
		}
	}
	var scanFailures []CaptureFailure
	var coverageGaps []CaptureFailure
	for index := range s.manifest.Roots {
		root := &s.manifest.Roots[index]
		if !root.Existed || root.Source == "" {
			continue
		}
		currentSource, resolveErr := filepath.EvalSymlinks(root.Path)
		if resolveErr != nil || filepath.Clean(currentSource) != filepath.Clean(root.Source) {
			message := "watched root no longer resolves to its snapshotted target"
			if resolveErr != nil {
				message += ": " + resolveErr.Error()
			}
			failure := CaptureFailure{Path: root.Path, Error: message}
			scanFailures = append(scanFailures, failure)
			s.manifest.Unscanned = append(s.manifest.Unscanned, failure)
			diffExcluded[failure.Path] = true
			root.Uncaptured[failure.Path] = failure.Error
			continue
		}
		entries, failures, err := scanMappedRootContext(ctx, root.Source, root.Path, s.manifest.Excluded)
		if err != nil {
			failure := CaptureFailure{Path: root.Path, Error: err.Error()}
			scanFailures = append(scanFailures, failure)
			s.manifest.Unscanned = append(s.manifest.Unscanned, failure)
			diffExcluded[failure.Path] = true
			if ctx.Err() == nil {
				root.Uncaptured[failure.Path] = failure.Error
			}
			continue
		}
		rootAfter[index] = entries
		rootScanned[index] = true
		rootDiffExcluded[index] = make(map[string]bool, len(failures))
		for path, entry := range entries {
			after[path] = entry
		}
		for _, failure := range failures {
			// A gap inside a watched root is session-specific even when the OS
			// reports it as a permission refusal. Only the wider background scan
			// may classify such a gap as routine for outcome presentation.
			failure.Category = ""
			scanFailures = append(scanFailures, failure)
			diffExcluded[failure.Path] = true
			rootDiffExcluded[index][failure.Path] = true
			root.Uncaptured[failure.Path] = failure.Error
		}
		for _, failure := range coverageFailures(entries, root.Uncaptured) {
			coverageGaps = append(coverageGaps, failure)
			root.Uncaptured[failure.Path] = failure.Error
		}
	}
	// A partial walk cannot establish absence. Diff only roots whose after-scan
	// completed, otherwise every unvisited baseline entry would be fabricated
	// as a deletion. The unavailable root remains explicit in Unscanned.
	beforeScanned := make(map[string]Entry)
	for index, root := range s.manifest.Roots {
		if !rootScanned[index] {
			continue
		}
		for path, entry := range root.Before {
			beforeScanned[path] = entry
		}
	}
	changes := diff(beforeScanned, after, diffExcluded, s.manifest.Roots, s.dir)
	coveragePrefixes := failurePrefixes(coverageGaps)
	coverageMessages := failureMessages(coverageGaps)
	for index := range changes {
		changes[index].UnrestorableReason = overlappingFailure(
			changes[index].Path, coveragePrefixes, coverageMessages,
		)
		if changes[index].UnrestorableReason == "" {
			changes[index].RestoreSource = RestoreSourceClone
		} else {
			changes[index].RestoreSource = RestoreSourceNone
		}
	}

	wideAfter := make(map[string]Entry)
	wideFailures := append([]CaptureFailure(nil), s.manifest.ScanFailures...)
	if s.manifest.ScanRoot != "" {
		scanStarted := time.Now()
		entries, failures, err := scanRootWithNamesContext(ctx,
			s.manifest.ScanRoot, s.manifest.ScanExcluded, s.manifest.ScanExcludedNames,
		)
		s.manifest.ScanAfterMillis = time.Since(scanStarted).Milliseconds()
		s.manifest.ScanAfterFiles = len(entries)
		if err != nil {
			wideFailures = append(wideFailures, CaptureFailure{Path: s.manifest.ScanRoot, Error: err.Error()})
		} else {
			wideAfter = entries
			wideFailures = append(wideFailures, failures...)
		}
	}
	wideExcluded := make(map[string]bool)
	for _, failure := range wideFailures {
		if failure.Path != "" {
			wideExcluded[failure.Path] = true
		}
	}
	wideChanges := diff(s.scanBefore, wideAfter, wideExcluded, nil, "")
	changeIndexes := make(map[string]int, len(changes))
	for index := range changes {
		changeIndexes[changes[index].Path] = index
	}
	var wideChangeIndexes []int
	for index, root := range s.manifest.Roots {
		if !rootScanned[index] || len(initialUncaptured[index]) == 0 {
			continue
		}
		observedBefore := entriesCoveredBy(root.UncapturedBefore, initialUncaptured[index])
		observedAfter := entriesCoveredBy(rootAfter[index], initialUncaptured[index])
		for _, change := range diff(observedBefore, observedAfter, rootDiffExcluded[index], nil, "") {
			if _, exists := changeIndexes[change.Path]; exists {
				continue
			}
			change.VolumeSnapshotPath = physicalPathForRoot(root, change.Path)
			changeIndexes[change.Path] = len(changes)
			changes = append(changes, change)
			wideChangeIndexes = append(wideChangeIndexes, len(changes)-1)
		}
	}
	for _, change := range wideChanges {
		change, cloneCovered := mapWideChangeToClonePath(change, s.manifest.Roots)
		if _, exists := changeIndexes[change.Path]; exists {
			continue
		}
		if cloneCovered {
			continue
		}
		changeIndexes[change.Path] = len(changes)
		changes = append(changes, change)
		wideChangeIndexes = append(wideChangeIndexes, len(changes)-1)
	}
	classificationFailures := classifyWideChanges(
		ctx, changes, wideChangeIndexes, s.platform, &s.manifest.Backstop, progress,
	)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	s.manifest.ScanFailures = mergeFailures(wideFailures)
	allFailures := mergeFailures(scanFailures, coverageGaps, s.manifest.ScanFailures, classificationFailures)
	s.manifest.PostSessionFailures = append([]CaptureFailure(nil), allFailures...)
	s.manifest.EndedAt = now.UTC()
	s.manifest.After = after
	s.manifest.Changes = changes
	s.manifest.Complete = len(allFailures) == 0
	if len(allFailures) > 0 {
		s.manifest.Error = formatFailures("post-session coverage incomplete", allFailures)
	}
	if ctx.Err() != nil {
		s.manifest.Complete = false
		s.manifest.Interrupted = true
		s.manifest.InterruptedPhase = interruptedPhase
		s.manifest.StorageExact = false
		s.manifest.Error = joinText(s.manifest.Error, "measure retained snapshot after interrupted seal: "+ctx.Err().Error())
	}
	if err := writeManifest(s.dir, s.manifest); err != nil {
		s.manifest.Complete = false
		s.manifest.Error = joinText(s.manifest.Error, err.Error())
	}
	availableAfter, measuredAfter, measureAfterErr := int64(0), false, error(nil)
	if !s.manifest.Interrupted {
		availableAfter, measuredAfter, measureAfterErr = filesystemAvailableBytes(s.dir)
	}
	if s.manifest.Interrupted {
		// The first published sealed manifest already contains the interruption.
	} else if measureBeforeErr != nil || measureAfterErr != nil {
		measureErr := errors.Join(measureBeforeErr, measureAfterErr)
		s.manifest.Complete = false
		s.manifest.Error = joinText(s.manifest.Error, "measure retained snapshot: "+measureErr.Error())
	} else if s.manifest.StorageExact && measuredBefore && measuredAfter {
		s.manifest.StorageBytes += consumedBytes(availableBefore, availableAfter)
		if manifestBytes, err := allocatedPath(filepath.Join(s.dir, "manifest.json")); err != nil {
			s.manifest.Complete = false
			s.manifest.Error = joinText(s.manifest.Error, "measure snapshot manifest allocation: "+err.Error())
		} else if manifestBytes > s.manifest.StorageBytes {
			s.manifest.StorageBytes = manifestBytes
		}
		if err := writeManifest(s.dir, s.manifest); err != nil {
			s.manifest.Complete = false
			s.manifest.Error = joinText(s.manifest.Error, err.Error())
		}
	} else {
		s.manifest.StorageExact = false
		if storageBytes, err := allocatedTree(s.dir); err != nil {
			s.manifest.Complete = false
			s.manifest.Error = joinText(s.manifest.Error, "measure retained snapshot upper bound: "+err.Error())
		} else {
			s.manifest.StorageBytes = storageBytes
			if err := writeManifest(s.dir, s.manifest); err != nil {
				s.manifest.Complete = false
				s.manifest.Error = joinText(s.manifest.Error, err.Error())
			}
		}
	}
	s.summary.Changes = changes
	s.summary.Uncaptured = manifestFailures(s.manifest)
	s.summary.Unscanned = append([]CaptureFailure(nil), s.manifest.Unscanned...)
	s.summary.PostSessionFailures = append([]CaptureFailure(nil), s.manifest.PostSessionFailures...)
	s.summary.Complete = s.manifest.Complete
	s.summary.Interrupted = s.manifest.Interrupted
	s.summary.InterruptedPhase = s.manifest.InterruptedPhase
	s.summary.Error = s.manifest.Error
	s.summary.StorageBytes = s.manifest.StorageBytes
	s.summary.StorageExact = s.manifest.StorageExact
	s.summary.ScanFailures = append([]CaptureFailure(nil), s.manifest.ScanFailures...)
	s.summary.ScanAfterFiles = s.manifest.ScanAfterFiles
	s.summary.ScanAfterMillis = s.manifest.ScanAfterMillis
	s.summary.Backstop = s.manifest.Backstop
	return s.summary
}

func mapWideChangeToClonePath(change Change, roots []rootManifest) (Change, bool) {
	originalPath := change.Path
	matchedRoot := -1
	matchedPrefix := ""
	for index, root := range roots {
		for _, candidate := range []string{root.Path, root.Source} {
			if candidate == "" || (change.Path != candidate && !strings.HasPrefix(change.Path, candidate+string(os.PathSeparator))) {
				continue
			}
			if len(candidate) > len(matchedPrefix) {
				matchedRoot = index
				matchedPrefix = candidate
			}
		}
	}
	if matchedRoot < 0 {
		return change, false
	}
	root := roots[matchedRoot]
	relative, err := filepath.Rel(matchedPrefix, change.Path)
	if err == nil && relative != "." {
		change.Path = filepath.Join(root.Path, relative)
	} else {
		change.Path = root.Path
	}
	if filepath.Clean(change.Path) != filepath.Clean(originalPath) {
		change.VolumeSnapshotPath = originalPath
	}
	if !root.Existed {
		return change, false
	}
	uncaptured := make(map[string]bool, len(root.Uncaptured))
	for path := range root.Uncaptured {
		uncaptured[path] = true
	}
	return change, !coveredBy(change.Path, uncaptured)
}

type timeMachineExclusionCheck struct {
	excluded bool
	err      error
}

const timeMachineExclusionBatchSize = 256

func classifyWideChanges(
	ctx context.Context,
	changes []Change,
	indexes []int,
	platform VolumeSnapshotPlatform,
	backstop *Backstop,
	progress func(SealProgress),
) []CaptureFailure {
	checks := make(map[string]timeMachineExclusionCheck)
	pending := make(map[string]bool)
	for _, index := range indexes {
		if path, ok := exclusionCheckPath(changes[index], backstop); ok {
			pending[path] = true
		}
	}
	total := len(pending)
	completed := 0
	report := func() {
		if progress != nil {
			progress(SealProgress{Phase: "time-machine-inclusion", Completed: completed, Total: total})
		}
	}
	if total > 0 {
		report()
	}
	var failures []CaptureFailure
	for len(pending) > 0 {
		paths := make([]string, 0, len(pending))
		minimumDepth := -1
		for path := range pending {
			if _, _, inherited := inheritedExcludedCheck(path, checks); inherited {
				delete(pending, path)
				completed++
				continue
			}
			depth := pathDepth(path)
			if minimumDepth < 0 || depth < minimumDepth {
				minimumDepth = depth
				paths = paths[:0]
				paths = append(paths, path)
			} else if depth == minimumDepth {
				paths = append(paths, path)
			}
		}
		if len(pending) == 0 {
			report()
			break
		}
		sort.Strings(paths)
		var batchable []string
		for _, path := range paths {
			if safeForTimeMachineExclusionBatch(path) {
				batchable = append(batchable, path)
				continue
			}
			check, failure := checkTimeMachineExclusionPath(ctx, platform, path)
			checks[path] = check
			delete(pending, path)
			completed++
			if failure != nil {
				failures = append(failures, *failure)
			}
			report()
		}
		for start := 0; start < len(batchable); start += timeMachineExclusionBatchSize {
			end := start + timeMachineExclusionBatchSize
			if end > len(batchable) {
				end = len(batchable)
			}
			batch := batchable[start:end]
			results, err := platform.IsExcludedBatch(ctx, batch)
			if err == nil && len(results) != len(batch) {
				err = fmt.Errorf("Time Machine inclusion batch returned %d results for %d paths", len(results), len(batch))
			}
			if err != nil {
				for _, path := range batch {
					check := timeMachineExclusionCheck{}
					var failure *CaptureFailure
					if ctxErr := ctx.Err(); ctxErr != nil {
						check.err = ctxErr
						failure = &CaptureFailure{
							Path:  path,
							Error: "Time Machine inclusion check stopped before this path could be verified: " + ctxErr.Error(),
						}
					} else {
						check, failure = checkTimeMachineExclusionPath(ctx, platform, path)
					}
					checks[path] = check
					delete(pending, path)
					completed++
					if failure != nil {
						failures = append(failures, *failure)
					}
				}
				report()
				continue
			}
			for index, path := range batch {
				check := timeMachineExclusionCheck{excluded: results[index]}
				checks[path] = check
				delete(pending, path)
				completed++
			}
			report()
		}
	}
	for _, index := range indexes {
		classifyWideChange(&changes[index], platform, backstop, checks)
	}
	return mergeFailures(failures)
}

func safeForTimeMachineExclusionBatch(path string) bool {
	if strings.ContainsAny(path, "\r\n") {
		return false
	}
	for _, component := range strings.Split(filepath.Clean(path), string(os.PathSeparator)) {
		for _, character := range component {
			if unicode.IsSpace(character) {
				return false
			}
		}
	}
	return true
}

func checkTimeMachineExclusionPath(
	ctx context.Context,
	platform VolumeSnapshotPlatform,
	path string,
) (timeMachineExclusionCheck, *CaptureFailure) {
	results, err := platform.IsExcludedBatch(ctx, []string{path})
	if err == nil && len(results) != 1 {
		err = fmt.Errorf("Time Machine inclusion check returned %d results for one path", len(results))
	}
	check := timeMachineExclusionCheck{err: err}
	if err == nil {
		check.excluded = results[0]
		return check, nil
	}
	failure := &CaptureFailure{
		Path:  path,
		Error: "Time Machine inclusion check failed for this path: " + err.Error(),
	}
	return check, failure
}

func exclusionCheckPath(change Change, backstop *Backstop) (string, bool) {
	entry := change.Before
	if entry == nil {
		entry = change.After
	}
	if entry != nil && (entry.Type == "other" || entry.Type == "file" && entry.Links > 1) {
		return "", false
	}
	if !backstop.Available {
		return "", false
	}
	if change.After == nil {
		return nearestExistingAncestor(filepath.Dir(change.Path)), true
	}
	return change.Path, true
}

func pathDepth(path string) int {
	depth := 0
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		depth++
		parent := filepath.Dir(current)
		if parent == current {
			return depth
		}
	}
}

func classifyWideChange(change *Change, platform VolumeSnapshotPlatform, backstop *Backstop, checks map[string]timeMachineExclusionCheck) {
	change.RestoreSource = RestoreSourceNone
	entry := change.Before
	if entry == nil {
		entry = change.After
	}
	if entry != nil {
		switch {
		case entry.Type == "other":
			change.UnrestorableReason = "unsupported file type cannot be restored per path"
			return
		case entry.Type == "file" && entry.Links > 1:
			change.UnrestorableReason = "hard-linked file cannot be restored per path without breaking its link group"
			return
		}
	}
	if !backstop.Available {
		change.UnrestorableReason = "no whole-volume snapshot was taken for this session"
		return
	}
	checkPath := change.Path
	if change.After == nil {
		checkPath = nearestExistingAncestor(filepath.Dir(change.Path))
	}
	check, ok := checks[checkPath]
	checkedPath := checkPath
	if !ok {
		check, checkedPath, ok = inheritedExcludedCheck(checkPath, checks)
	}
	if !ok {
		check.excluded, check.err = platform.IsExcluded(checkPath)
		checks[checkPath] = check
		checkedPath = checkPath
	}
	if check.err != nil {
		change.UnrestorableReason = "could not verify that the path was included in the Time Machine backstop: " + check.err.Error()
		return
	}
	if check.excluded {
		failure := CaptureFailure{Path: checkedPath, Error: "excluded from the Time Machine backup"}
		backstop.Excluded = mergeFailures(backstop.Excluded, []CaptureFailure{failure})
		change.UnrestorableReason = "path was excluded from the Time Machine backup"
		return
	}
	snapshotPath := change.Path
	if change.VolumeSnapshotPath != "" {
		snapshotPath = change.VolumeSnapshotPath
	}
	snapshot, covered := snapshotForPath(platform, *backstop, snapshotPath)
	if !covered {
		change.UnrestorableReason = "the path's volume has no recorded local snapshot for this session"
		return
	}
	change.RestoreSource = RestoreSourceVolume
	change.VolumeSnapshot = &snapshot
}

func inheritedExcludedCheck(path string, checks map[string]timeMachineExclusionCheck) (timeMachineExclusionCheck, string, bool) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if check, ok := checks[current]; ok && check.excluded && check.err == nil {
			return check, current, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return timeMachineExclusionCheck{}, "", false
}

func nearestExistingAncestor(path string) string {
	path = filepath.Clean(path)
	for {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func entriesCoveredBy(entries map[string]Entry, prefixes map[string]bool) map[string]Entry {
	covered := make(map[string]Entry)
	for path, entry := range entries {
		if coveredBy(path, prefixes) {
			covered[path] = entry
		}
	}
	return covered
}

func physicalPathForRoot(root rootManifest, logicalPath string) string {
	relative, err := filepath.Rel(root.Path, logicalPath)
	if err != nil || relative == "." {
		return root.Source
	}
	return filepath.Join(root.Source, relative)
}

func coverageFailures(entries map[string]Entry, alreadyUncaptured map[string]string) []CaptureFailure {
	known := make(map[string]bool, len(alreadyUncaptured))
	for path := range alreadyUncaptured {
		known[path] = true
	}
	var failures []CaptureFailure
	for path, entry := range entries {
		if coveredBy(path, known) {
			continue
		}
		switch {
		case entry.Type == "other":
			failures = append(failures, CaptureFailure{Path: path, Error: UnsupportedFileTypeCoverageReason})
		case entry.Type == "file" && entry.Links > 1:
			failures = append(failures, CaptureFailure{Path: path, Error: "hard-linked files are outside snapshot coverage; restoring one path could silently break the link group"})
		case entry.Type == "symlink":
			if target, err := os.Stat(path); err == nil && target.IsDir() {
				failures = append(failures, CaptureFailure{Path: path, Error: "symlinked directory target is not followed or snapshotted"})
			}
		}
	}
	return failures
}

// Snapshot returns the session's current detached summary.
func (s *Session) Snapshot() Summary {
	data, _ := json.Marshal(s.summary)
	var copied Summary
	_ = json.Unmarshal(data, &copied)
	return copied
}

func captureRoot(root, destination string, excluded []string) (rootManifest, []CaptureFailure, map[string]bool, int64, int64, error) {
	return captureRootContext(context.Background(), root, destination, excluded)
}

func captureRootContext(ctx context.Context, root, destination string, excluded []string) (rootManifest, []CaptureFailure, map[string]bool, int64, int64, error) {
	state := rootManifest{Path: root, Before: make(map[string]Entry), Uncaptured: make(map[string]string)}
	methods := make(map[string]bool)
	source, resolveErr := filepath.EvalSymlinks(root)
	if errors.Is(resolveErr, os.ErrNotExist) {
		failure := CaptureFailure{Path: root, Error: "watched path does not exist"}
		state.Uncaptured[root] = failure.Error
		return state, []CaptureFailure{failure}, methods, 0, 0, nil
	}
	if resolveErr != nil {
		failure := CaptureFailure{Path: root, Error: "resolve watched path: " + resolveErr.Error()}
		state.Uncaptured[root] = failure.Error
		return state, []CaptureFailure{failure}, methods, 0, 0, nil
	}
	source = filepath.Clean(source)
	state.Source = source
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		failure := CaptureFailure{Path: root, Error: "watched path does not exist"}
		state.Uncaptured[root] = failure.Error
		return state, []CaptureFailure{failure}, methods, 0, 0, nil
	}
	if err != nil {
		failure := CaptureFailure{Path: root, Error: err.Error()}
		state.Uncaptured[root] = err.Error()
		return state, []CaptureFailure{failure}, methods, 0, 0, nil
	}
	state.Existed = true
	// clonefile assigns fresh inode ctimes to the clone, so source ctime cannot
	// be reconstructed by scanning the clone alone. Bracket capture with source
	// metadata guards and admit a baseline only when the captured membership,
	// type, link target, and file size match that stable source state. Snapshot
	// bytes and the source metadata baseline therefore describe the same instant.
	sourceGuardBefore, beforeFailures, err := scanMappedRootContext(ctx, source, root, excluded)
	if err != nil {
		return state, beforeFailures, methods, 0, 0, err
	}

	var captureFailures []CaptureFailure
	var copiedBytes int64
	if err := ctx.Err(); err != nil {
		return state, beforeFailures, methods, 0, 0, err
	}
	if info.IsDir() && !isExcluded(source, excluded) && !containsExcludedPath(source, excluded) {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return state, beforeFailures, methods, 0, 0, err
		}
		if method, err := cloneDirectory(source, destination); err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return state, beforeFailures, methods, 0, 0, ctxErr
			}
			methods[method] = true
		} else {
			if err := removeSnapshotTree(destination); err != nil {
				return state, beforeFailures, methods, 0, 0, fmt.Errorf("remove failed recursive clone: %w", err)
			}
			var fallbackMethods map[string]bool
			captureFailures, fallbackMethods, copiedBytes, err = captureIndividuallyContext(ctx, source, destination, excluded)
			captureFailures = translateFailures(captureFailures, source, root)
			if err != nil {
				return state, mergeFailures(beforeFailures, captureFailures), methods, 0, copiedBytes, err
			}
			for method := range fallbackMethods {
				methods[method] = true
			}
		}
	} else {
		var fallbackMethods map[string]bool
		captureFailures, fallbackMethods, copiedBytes, err = captureIndividuallyContext(ctx, source, destination, excluded)
		captureFailures = translateFailures(captureFailures, source, root)
		if err != nil {
			return state, mergeFailures(beforeFailures, captureFailures), methods, 0, copiedBytes, err
		}
		for method := range fallbackMethods {
			methods[method] = true
		}
	}

	snapshotEntries, snapshotFailures, err := scanSnapshotRootContext(ctx, destination, root)
	if err != nil {
		return state, mergeFailures(beforeFailures, captureFailures, snapshotFailures), methods, 0, copiedBytes, err
	}
	sourceGuardAfter, afterFailures, err := scanMappedRootContext(ctx, source, root, excluded)
	if err != nil {
		return state, mergeFailures(beforeFailures, captureFailures, snapshotFailures, afterFailures), methods, 0, copiedBytes, err
	}
	captured, failures := reconcileCapture(sourceGuardBefore, sourceGuardAfter, snapshotEntries,
		mergeFailures(beforeFailures, captureFailures, afterFailures, snapshotFailures))
	stableObserved := stableObservedEntries(sourceGuardBefore, sourceGuardAfter)
	state.UncapturedBefore = entriesCoveredBy(stableObserved, failurePrefixes(failures))
	for _, failure := range failures {
		state.Uncaptured[failure.Path] = failure.Error
	}
	state.Before = captured
	return state, failures, methods, logicalSize(state.Before), copiedBytes, err
}

func stableObservedEntries(before, after map[string]Entry) map[string]Entry {
	stable := make(map[string]Entry)
	for path, entry := range before {
		if afterEntry, exists := after[path]; exists && afterEntry == entry {
			stable[path] = entry
		}
	}
	return stable
}

func scanMappedRoot(sourceRoot, reportedRoot string, excluded []string) (map[string]Entry, []CaptureFailure, error) {
	return scanMappedRootContext(context.Background(), sourceRoot, reportedRoot, excluded)
}

func scanMappedRootContext(ctx context.Context, sourceRoot, reportedRoot string, excluded []string) (map[string]Entry, []CaptureFailure, error) {
	entries, failures, err := scanRootWithNamesContext(ctx, sourceRoot, excluded, nil)
	translated := make(map[string]Entry, len(entries))
	for path, entry := range entries {
		relative, relErr := filepath.Rel(sourceRoot, path)
		if relErr != nil {
			failures = append(failures, CaptureFailure{Path: reportedRoot, Error: relErr.Error()})
			continue
		}
		reported := reportedRoot
		if relative != "." {
			reported = filepath.Join(reportedRoot, relative)
		}
		translated[reported] = entry
	}
	return translated, translateFailures(failures, sourceRoot, reportedRoot), err
}

func translateFailures(failures []CaptureFailure, sourceRoot, reportedRoot string) []CaptureFailure {
	translated := make([]CaptureFailure, 0, len(failures))
	for _, failure := range failures {
		relative, err := filepath.Rel(sourceRoot, failure.Path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			failure.Path = reportedRoot
			if relative != "." {
				failure.Path = filepath.Join(reportedRoot, relative)
			}
		}
		translated = append(translated, failure)
	}
	return translated
}

func scanSnapshotRoot(snapshotRoot, originalRoot string) (map[string]Entry, []CaptureFailure, error) {
	return scanSnapshotRootContext(context.Background(), snapshotRoot, originalRoot)
}

func scanSnapshotRootContext(ctx context.Context, snapshotRoot, originalRoot string) (map[string]Entry, []CaptureFailure, error) {
	entries, failures, err := scanRootWithNamesContext(ctx, snapshotRoot, nil, nil)
	translated := make(map[string]Entry, len(entries))
	for path, entry := range entries {
		relative, relErr := filepath.Rel(snapshotRoot, path)
		if relErr != nil {
			failures = append(failures, CaptureFailure{Path: originalRoot, Error: relErr.Error()})
			continue
		}
		original := originalRoot
		if relative != "." {
			original = filepath.Join(originalRoot, relative)
		}
		translated[original] = entry
	}
	for index := range failures {
		if relative, relErr := filepath.Rel(snapshotRoot, failures[index].Path); relErr == nil {
			failures[index].Path = filepath.Join(originalRoot, relative)
		}
	}
	return translated, failures, err
}

func reconcileCapture(
	before, after, snapshot map[string]Entry,
	existingFailures []CaptureFailure,
) (map[string]Entry, []CaptureFailure) {
	captured := make(map[string]Entry)
	failures := append([]CaptureFailure(nil), existingFailures...)
	failed := failurePrefixes(failures)
	paths := make(map[string]bool)
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	for path := range snapshot {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		if coveredBy(path, failed) {
			continue
		}
		beforeEntry, beforeExists := before[path]
		afterEntry, afterExists := after[path]
		snapshotEntry, snapshotExists := snapshot[path]
		if !beforeExists || !afterExists || beforeEntry != afterEntry {
			failures = append(failures, CaptureFailure{Path: path, Error: "path changed while its snapshot was being captured"})
			failed[path] = true
			continue
		}
		if !snapshotExists || !snapshotMatchesSource(snapshotEntry, beforeEntry) {
			failures = append(failures, CaptureFailure{Path: path, Error: "captured path does not match the stable source metadata"})
			failed[path] = true
			continue
		}
		switch {
		case beforeEntry.Type == "other":
			failures = append(failures, CaptureFailure{Path: path, Error: UnsupportedFileTypeCoverageReason})
			failed[path] = true
		case beforeEntry.Type == "file" && beforeEntry.Links > 1:
			failures = append(failures, CaptureFailure{Path: path, Error: "hard-linked files are outside snapshot coverage; restoring one path could silently break the link group"})
			failed[path] = true
		case beforeEntry.Type == "symlink":
			if target, statErr := os.Stat(path); statErr == nil && target.IsDir() {
				failures = append(failures, CaptureFailure{Path: path, Error: "symlinked directory target is not followed or snapshotted"})
				failed[path] = true
			}
		}
		if !failed[path] {
			captured[path] = beforeEntry
		}
	}
	return captured, mergeFailures(failures)
}

func snapshotMatchesSource(snapshot, source Entry) bool {
	if snapshot.Type != source.Type || snapshot.LinkTarget != source.LinkTarget {
		return false
	}
	if source.Type == "file" && snapshot.Size != source.Size {
		return false
	}
	return true
}

func failurePrefixes(failures []CaptureFailure) map[string]bool {
	prefixes := make(map[string]bool, len(failures))
	for _, failure := range failures {
		prefixes[failure.Path] = true
	}
	return prefixes
}

func failureMessages(failures []CaptureFailure) map[string]string {
	messages := make(map[string]string, len(failures))
	for _, failure := range failures {
		messages[failure.Path] = failure.Error
	}
	return messages
}

func overlappingFailure(path string, prefixes map[string]bool, messages map[string]string) string {
	var matched string
	for prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+string(os.PathSeparator)) ||
			strings.HasPrefix(prefix, path+string(os.PathSeparator)) {
			if matched == "" || len(prefix) < len(matched) {
				matched = prefix
			}
		}
	}
	if matched == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", matched, messages[matched])
}

func manifestFailures(value manifest) []CaptureFailure {
	var failures []CaptureFailure
	for _, root := range value.Roots {
		for path, message := range root.Uncaptured {
			failures = append(failures, CaptureFailure{Path: path, Error: message})
		}
	}
	return mergeFailures(failures)
}

func captureIndividually(root, destination string, excluded []string) ([]CaptureFailure, map[string]bool, int64, error) {
	return captureIndividuallyContext(context.Background(), root, destination, excluded)
}

func captureIndividuallyContext(ctx context.Context, root, destination string, excluded []string) ([]CaptureFailure, map[string]bool, int64, error) {
	methods := make(map[string]bool)
	var failures []CaptureFailure
	var copiedBytes int64
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, methods, 0, err
	}
	if !rootInfo.IsDir() {
		method, copied, err := captureOne(root, destination, rootInfo)
		if err != nil {
			return []CaptureFailure{{Path: root, Error: err.Error()}}, methods, 0, nil
		}
		methods[method] = true
		if copied {
			copiedBytes += rootInfo.Size()
		}
		return failures, methods, copiedBytes, nil
	}

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: walkErr.Error()})
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcluded(path, excluded) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: relErr.Error()})
			return nil
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: infoErr.Error()})
			return nil
		}
		method, copied, captureErr := captureOne(path, target, info)
		if captureErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: captureErr.Error()})
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if method != "" {
			methods[method] = true
		}
		if copied {
			copiedBytes += info.Size()
		}
		return nil
	})
	return failures, methods, copiedBytes, err
}

func captureOne(source, destination string, info fs.FileInfo) (string, bool, error) {
	switch {
	case info.IsDir():
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return "", false, err
		}
		return "", false, nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(source)
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", false, err
		}
		if err := os.Symlink(target, destination); err != nil {
			return "", false, err
		}
		return "metadata", false, nil
	case info.Mode().IsRegular():
		if err := readable(source); err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", false, err
		}
		method, copied, err := cloneOrCopyFile(source, destination, info.Mode().Perm())
		if err != nil {
			return "", false, err
		}
		return method, copied, nil
	default:
		return "", false, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
}

func scanRoot(root string, excluded []string) (map[string]Entry, []CaptureFailure, error) {
	return scanRootWithNamesContext(context.Background(), root, excluded, nil)
}

func scanRootWithNames(root string, excluded, excludedNames []string) (map[string]Entry, []CaptureFailure, error) {
	return scanRootWithNamesContext(context.Background(), root, excluded, excludedNames)
}

func scanRootWithNamesContext(ctx context.Context, root string, excluded, excludedNames []string) (map[string]Entry, []CaptureFailure, error) {
	entries := make(map[string]Entry)
	excludedNameSet := make(map[string]bool, len(excludedNames))
	for _, name := range excludedNames {
		excludedNameSet[name] = true
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil, nil
	}
	if err != nil {
		return entries, nil, err
	}
	if !info.IsDir() {
		entry, err := entryFromInfo(root, info)
		if err != nil {
			return entries, []CaptureFailure{failureFromError(root, err)}, nil
		}
		entries[root] = entry
		return entries, nil, nil
	}
	var failures []CaptureFailure
	err = filepath.WalkDir(root, func(path string, directoryEntry fs.DirEntry, walkErr error) error {
		observeScanPath(ctx, path)
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			failures = append(failures, failureFromError(path, walkErr))
			if directoryEntry != nil && directoryEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcluded(path, excluded) {
			if directoryEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != root && directoryEntry.IsDir() && excludedNameSet[directoryEntry.Name()] {
			return filepath.SkipDir
		}
		info, infoErr := directoryEntry.Info()
		if infoErr != nil {
			failures = append(failures, failureFromError(path, infoErr))
			return nil
		}
		entry, entryErr := entryFromInfo(path, info)
		if entryErr != nil {
			failures = append(failures, failureFromError(path, entryErr))
			return nil
		}
		entries[path] = entry
		return nil
	})
	return entries, failures, err
}

func failureFromError(path string, err error) CaptureFailure {
	failure := CaptureFailure{Path: path, Error: err.Error()}
	if os.IsPermission(err) {
		failure.Category = CaptureFailureCategoryRoutinePermission
	}
	return failure
}

func isExcluded(path string, excluded []string) bool {
	path = filepath.Clean(path)
	for _, exclusion := range excluded {
		exclusion = filepath.Clean(exclusion)
		if path == exclusion || strings.HasPrefix(path, exclusion+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func containsExcludedPath(root string, excluded []string) bool {
	root = filepath.Clean(root)
	for _, exclusion := range excluded {
		exclusion = filepath.Clean(exclusion)
		if exclusion == root || strings.HasPrefix(exclusion, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func entryFromInfo(path string, info fs.FileInfo) (Entry, error) {
	entry := Entry{
		Size: info.Size(), MTime: info.ModTime().UnixNano(), CTime: changeTime(info),
		Mode: uint32(info.Mode()), Type: "file", Links: linkCount(info),
	}
	if info.IsDir() {
		entry.Type = "directory"
	} else if info.Mode()&os.ModeSymlink != 0 {
		entry.Type = "symlink"
		target, err := os.Readlink(path)
		if err != nil {
			return Entry{}, err
		}
		entry.LinkTarget = target
	} else if !info.Mode().IsRegular() {
		entry.Type = "other"
	}
	return entry, nil
}

func diff(before, after map[string]Entry, uncaptured map[string]bool, roots []rootManifest, snapshotRoot string) []Change {
	changes := make([]Change, 0)
	for path, oldEntry := range before {
		if excludedFromDiff(path, uncaptured, roots) {
			continue
		}
		newEntry, exists := after[path]
		if !exists {
			beforeCopy := oldEntry
			changes = append(changes, Change{Kind: "deleted", Path: path, Before: &beforeCopy})
			continue
		}
		if entriesDiffer(oldEntry, newEntry) &&
			!cheaplyMatchesSnapshot(path, oldEntry, newEntry, roots, snapshotRoot) {
			beforeCopy, afterCopy := oldEntry, newEntry
			changes = append(changes, Change{Kind: "modified", Path: path, Before: &beforeCopy, After: &afterCopy})
		}
	}
	for path, newEntry := range after {
		if _, existed := before[path]; existed || excludedFromDiff(path, uncaptured, roots) {
			continue
		}
		afterCopy := newEntry
		changes = append(changes, Change{Kind: "created", Path: path, After: &afterCopy})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// cheaplyMatchesSnapshot suppresses metadata-only false positives only when a
// clone-backed byte comparison is bounded and conclusive. A missing clone, a
// read error, or a file above the bound deliberately falls back to reporting
// the metadata change.
func cheaplyMatchesSnapshot(path string, before, after Entry, roots []rootManifest, snapshotRoot string) bool {
	if snapshotRoot == "" || before.Type != "file" || after.Type != "file" ||
		before.Size != after.Size || before.Size > AutomaticContentComparisonBytes ||
		before.Mode != after.Mode || before.Links != after.Links {
		return false
	}
	value := manifest{Roots: roots}
	snapshotPath, err := snapshotPathFor(value, snapshotRoot, path)
	if err != nil {
		return false
	}
	equal, err := filesEqual(snapshotPath, path)
	return err == nil && equal
}

func excludedFromDiff(path string, uncaptured map[string]bool, roots []rootManifest) bool {
	if !coveredBy(path, uncaptured) {
		return false
	}
	for _, root := range roots {
		if !root.Existed || (path != root.Path && !strings.HasPrefix(path, root.Path+string(os.PathSeparator))) {
			continue
		}
		coveredByRootFailure := false
		for failure := range root.Uncaptured {
			if path == failure || strings.HasPrefix(path, failure+string(os.PathSeparator)) {
				coveredByRootFailure = true
				break
			}
		}
		if !coveredByRootFailure {
			return false
		}
	}
	return true
}

func entriesDiffer(before, after Entry) bool {
	if before.Type == "directory" && after.Type == "directory" {
		return before.Mode != after.Mode
	}
	return before != after
}

func coveredBy(path string, prefixes map[string]bool) bool {
	for prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func normalizeRoots(paths []string) ([]string, error) {
	unique := make(map[string]bool)
	var roots []string
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		absolute = filepath.Clean(absolute)
		if unique[absolute] {
			continue
		}
		unique[absolute] = true
		roots = append(roots, absolute)
	}
	sort.Strings(roots)
	filtered := roots[:0]
	for _, candidate := range roots {
		covered := false
		for _, parent := range filtered {
			if candidate == parent {
				covered = true
				break
			}
			if !strings.HasPrefix(candidate, parent+string(os.PathSeparator)) {
				continue
			}
			resolvedParent, parentErr := resolveExistingPath(parent)
			resolvedCandidate, candidateErr := resolveExistingPath(candidate)
			if parentErr == nil && candidateErr == nil &&
				(resolvedCandidate == resolvedParent || strings.HasPrefix(resolvedCandidate, resolvedParent+string(os.PathSeparator))) {
				covered = true
				break
			}
		}
		if !covered {
			filtered = append(filtered, candidate)
		}
	}
	return filtered, nil
}

func overlappingRootExclusions(roots []string) []string {
	unique := make(map[string]bool)
	var exclusions []string
	for _, candidate := range roots {
		for _, parent := range roots {
			if candidate == parent || !strings.HasPrefix(candidate, parent+string(os.PathSeparator)) {
				continue
			}
			resolvedParent, parentErr := resolveExistingPath(parent)
			resolvedCandidate, candidateErr := resolveExistingPath(candidate)
			if parentErr != nil || candidateErr != nil || resolvedCandidate == resolvedParent ||
				strings.HasPrefix(resolvedCandidate, resolvedParent+string(os.PathSeparator)) {
				continue
			}
			relative, err := filepath.Rel(parent, candidate)
			if err != nil {
				continue
			}
			logical := parent
			physical := resolvedParent
			for _, component := range strings.Split(relative, string(os.PathSeparator)) {
				logical = filepath.Join(logical, component)
				physical = filepath.Join(physical, component)
				info, err := os.Lstat(logical)
				if err == nil && info.Mode()&os.ModeSymlink != 0 {
					resolvedBoundary, err := resolveExistingPath(logical)
					if err != nil || resolvedBoundary != resolvedCandidate {
						break
					}
					if !unique[physical] {
						unique[physical] = true
						exclusions = append(exclusions, physical)
					}
					break
				}
			}
		}
	}
	sort.Strings(exclusions)
	return exclusions
}

func projectRoot(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for candidate := absolute; ; candidate = filepath.Dir(candidate) {
		if _, err := os.Lstat(filepath.Join(candidate, ".git")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return absolute, nil
		}
	}
}

func logicalSize(entries map[string]Entry) int64 {
	var total int64
	for _, entry := range entries {
		if entry.Type == "file" {
			total += entry.Size
		}
	}
	return total
}

func allocatedTree(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += allocatedSize(info)
		return nil
	})
	return total, err
}

func allocatedPath(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return allocatedSize(info), nil
}

func consumedBytes(before, after int64) int64 {
	if after >= before {
		return 0
	}
	return before - after
}

func storageDescription(methods map[string]bool) string {
	if len(methods) == 0 {
		return "metadata-only"
	}
	keys := make([]string, 0, len(methods))
	for method := range methods {
		keys = append(keys, method)
	}
	sort.Strings(keys)
	return strings.Join(keys, "+")
}

func mergeFailures(groups ...[]CaptureFailure) []CaptureFailure {
	byPath := make(map[string]CaptureFailure)
	for _, group := range groups {
		for _, failure := range group {
			if existing, ok := byPath[failure.Path]; ok && existing.Category != failure.Category {
				failure.Category = ""
			}
			byPath[failure.Path] = failure
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]CaptureFailure, 0, len(paths))
	for _, path := range paths {
		out = append(out, byPath[path])
	}
	return out
}

func formatFailures(prefix string, failures []CaptureFailure) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.Path+": "+failure.Error)
	}
	return prefix + ": " + strings.Join(parts, "; ")
}

func joinText(existing, added string) string {
	if existing == "" {
		return added
	}
	if added == "" {
		return existing
	}
	return existing + "; " + added
}

func writeManifest(directory string, value manifest) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode file snapshot manifest: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create file snapshot manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, "manifest.json")); err != nil {
		return fmt.Errorf("publish file snapshot manifest: %w", err)
	}
	return nil
}

func readManifest(directory string) (manifest, error) {
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return manifest{}, err
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		return manifest{}, err
	}
	if value.Version != manifestVersion {
		return manifest{}, fmt.Errorf("unsupported file snapshot manifest version %d", value.Version)
	}
	return value, nil
}

// PlanRetention applies age and measured-allocation limits in one oldest-first
// pass. The newest stored session always survives. Unknown/inexact allocation
// is never used to justify cap eviction, but age expiry can still select it.
func PlanRetention(
	stateDir string,
	sessions []StoredSession,
	capBytes int64,
	maxAge time.Duration,
	now time.Time,
) (RetentionPlan, error) {
	return PlanRetentionContext(context.Background(), stateDir, sessions, capBytes, maxAge, now)
}

// PlanRetentionContext is PlanRetention with cancellation while waiting for
// retention/session locks and inspecting stored manifests.
func PlanRetentionContext(
	ctx context.Context,
	stateDir string,
	sessions []StoredSession,
	capBytes int64,
	maxAge time.Duration,
	now time.Time,
) (RetentionPlan, error) {
	if capBytes < 0 {
		return RetentionPlan{}, errors.New("snapshot retention cap cannot be negative")
	}
	unlockRetention, err := acquireSnapshotLockContext(ctx, stateDir, "retention", unix.LOCK_SH)
	if err != nil {
		return RetentionPlan{}, err
	}
	defer unlockRetention()
	return planRetentionWhileLockedContext(ctx, stateDir, sessions, capBytes, maxAge, now)
}

// PlanRetentionWhileLocked computes a retention plan while the caller holds
// the retention lock. It exists so ApplyRetentionRemovals can validate a saved
// destructive plan without dropping its exclusive lock.
func PlanRetentionWhileLocked(
	stateDir string,
	sessions []StoredSession,
	capBytes int64,
	maxAge time.Duration,
	now time.Time,
) (RetentionPlan, error) {
	if capBytes < 0 {
		return RetentionPlan{}, errors.New("snapshot retention cap cannot be negative")
	}
	return planRetentionWhileLockedContext(context.Background(), stateDir, sessions, capBytes, maxAge, now)
}

func planRetentionWhileLocked(
	stateDir string,
	sessions []StoredSession,
	capBytes int64,
	maxAge time.Duration,
	now time.Time,
) (RetentionPlan, error) {
	return planRetentionWhileLockedContext(context.Background(), stateDir, sessions, capBytes, maxAge, now)
}

func planRetentionWhileLockedContext(
	ctx context.Context,
	stateDir string,
	sessions []StoredSession,
	capBytes int64,
	maxAge time.Duration,
	now time.Time,
) (RetentionPlan, error) {
	type retained struct {
		StoredSession
		bytes           int64
		exact           bool
		accountingKnown bool
		hasSnapshot     bool
		damaged         bool
		auditRecord     bool
	}
	byID := make(map[string]*retained, len(sessions))
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return RetentionPlan{}, err
		}
		if session.ID == "" || strings.ContainsAny(session.ID, `/\\`) {
			return RetentionPlan{}, fmt.Errorf("plan retention: invalid session id %q", session.ID)
		}
		copy := &retained{
			StoredSession: session, bytes: session.StorageBytes,
			exact: session.StorageExact, accountingKnown: session.StorageKnown,
			auditRecord: true,
		}
		byID[session.ID] = copy
	}
	root := filepath.Join(stateDir, "snapshots")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RetentionPlan{}, fmt.Errorf("inspect retained snapshots: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return RetentionPlan{}, err
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		unlock, lockErr := acquireSnapshotLockContext(ctx, stateDir, entry.Name(), unix.LOCK_SH)
		if lockErr != nil {
			return RetentionPlan{}, lockErr
		}
		value, readErr := readManifest(filepath.Join(root, entry.Name()))
		unlock()
		if readErr != nil {
			item := byID[entry.Name()]
			if item == nil {
				started := time.Time{}
				if info, infoErr := entry.Info(); infoErr == nil {
					started = info.ModTime()
				}
				item = &retained{StoredSession: StoredSession{ID: entry.Name(), StartedAt: started}}
				byID[entry.Name()] = item
			}
			item.hasSnapshot = true
			item.damaged = true
			if !item.accountingKnown {
				item.exact = false
			}
			continue
		}
		item := byID[entry.Name()]
		if item == nil {
			item = &retained{StoredSession: StoredSession{ID: entry.Name(), StartedAt: value.StartedAt}}
			byID[entry.Name()] = item
		}
		item.bytes = value.StorageBytes
		item.exact = value.StorageExact
		item.accountingKnown = true
		item.hasSnapshot = true
	}
	ordered := make([]*retained, 0, len(byID))
	for _, item := range byID {
		if err := ctx.Err(); err != nil {
			return RetentionPlan{}, err
		}
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StartedAt.Equal(ordered[j].StartedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].StartedAt.Before(ordered[j].StartedAt)
	})
	plan := RetentionPlan{
		Before: Usage{CapBytes: capBytes, Exact: true},
		After:  Usage{CapBytes: capBytes, Exact: true},
	}
	var enforcedBytes int64
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return RetentionPlan{}, err
		}
		if !item.hasSnapshot {
			continue
		}
		if item.accountingKnown {
			plan.Before.Bytes += item.bytes
		}
		plan.Before.Sessions++
		if item.damaged {
			if item.accountingKnown && item.exact {
				plan.Warnings = append(plan.Warnings, RetentionWarning{
					SessionID: item.ID,
					Error:     "snapshot manifest is missing or unreadable; using measured bytes from its audit record for byte-cap accounting",
				})
			} else {
				plan.Warnings = append(plan.Warnings, RetentionWarning{
					SessionID: item.ID,
					Error:     "snapshot manifest is missing or unreadable; its bytes are unknown, so only age expiry can select this store, subject to the newest-session guard",
				})
			}
		}
		if item.accountingKnown && item.exact {
			enforcedBytes += item.bytes
		} else if !item.accountingKnown || !item.exact {
			plan.Before.Exact = false
		}
	}
	plan.After = plan.Before
	if len(ordered) == 0 {
		return plan, nil
	}
	protected := map[string]bool{ordered[len(ordered)-1].ID: true}
	for index := len(ordered) - 1; index >= 0; index-- {
		if ordered[index].auditRecord {
			protected[ordered[index].ID] = true
			break
		}
	}
	cutoff := now.Add(-maxAge)
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return RetentionPlan{}, err
		}
		expired := maxAge > 0 && item.StartedAt.Before(cutoff)
		capRequired := item.hasSnapshot && item.accountingKnown && item.exact && enforcedBytes > capBytes
		if protected[item.ID] || (!expired && !capRequired) {
			continue
		}
		plan.Removals = append(plan.Removals, RetentionRemoval{
			SessionID: item.ID, StartedAt: item.StartedAt,
			StorageBytes: item.bytes, StorageExact: item.accountingKnown && item.exact,
			HasSnapshot: item.hasSnapshot,
			Expired:     expired, CapRequired: capRequired,
		})
		if item.hasSnapshot {
			if item.accountingKnown {
				plan.After.Bytes -= item.bytes
			}
			plan.After.Sessions--
			if item.accountingKnown && item.exact {
				enforcedBytes -= item.bytes
			}
		}
	}
	return plan, nil
}

// ApplyRetentionRemovals removes planned clone stores one at a time while each
// session's exclusive lock is held. finalize updates or deletes the associated
// audit record under the same lock. completed is called after each coherent
// removal, so callers can report partial progress before a later error.
func ApplyRetentionRemovals(
	stateDir string,
	removals []RetentionRemoval,
	validate func() error,
	finalize func(RetentionRemoval) error,
	completed func(RetentionRemoval),
) error {
	return ApplyRetentionRemovalsContext(context.Background(), stateDir, removals, validate, finalize, completed)
}

// ApplyRetentionRemovalsContext is ApplyRetentionRemovals with cancellation
// checks while waiting for locks, walking clone trees, and removing entries.
func ApplyRetentionRemovalsContext(
	ctx context.Context,
	stateDir string,
	removals []RetentionRemoval,
	validate func() error,
	finalize func(RetentionRemoval) error,
	completed func(RetentionRemoval),
) error {
	for _, removal := range removals {
		if removal.SessionID == "" || strings.ContainsAny(removal.SessionID, `/\\`) {
			return fmt.Errorf("remove retained snapshots: invalid session id %q", removal.SessionID)
		}
	}
	unlockRetention, err := acquireSnapshotLockContext(ctx, stateDir, "retention", unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlockRetention()
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	for _, removal := range removals {
		if err := ctx.Err(); err != nil {
			return err
		}
		unlock, lockErr := acquireSnapshotLockContext(ctx, stateDir, removal.SessionID, unix.LOCK_EX)
		if lockErr != nil {
			return lockErr
		}
		if removal.HasSnapshot {
			path := filepath.Join(stateDir, "snapshots", removal.SessionID)
			removeErr := removeSnapshotTreeContext(ctx, path)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				unlock()
				return &RetentionApplyError{Removal: removal, Err: fmt.Errorf("remove retained snapshot %s: %w", removal.SessionID, removeErr)}
			}
		}
		if err := finalize(removal); err != nil {
			unlock()
			return &RetentionApplyError{
				Removal: removal, SnapshotRemoved: removal.HasSnapshot,
				Err: fmt.Errorf("finalize retained session %s: %w", removal.SessionID, err),
			}
		}
		unlock()
		if completed != nil {
			completed(removal)
		}
	}
	return nil
}

// removeSnapshotTree removes captured content even when its original
// directories were read-only. Unlinking a file does not require write access
// to the file itself, but walking and unlinking entries requires read, write,
// and execute access to their parent directories. WalkDir invokes the callback
// before reading a directory, so adding owner access there also handles nested
// read-only trees without following captured symlinks.
func removeSnapshotTree(path string) error {
	return removeSnapshotTreeContext(context.Background(), path)
}

func removeSnapshotTreeContext(ctx context.Context, path string) error {
	err := removeSnapshotEntryContext(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove snapshot tree: %w", err)
	}
	return nil
}

func removeSnapshotEntryContext(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	mode := info.Mode().Perm()
	if mode&0o700 != 0o700 {
		if err := os.Chmod(path, mode|0o700); err != nil {
			return fmt.Errorf("make snapshot directory removable: %w", err)
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeSnapshotEntryContext(ctx, filepath.Join(path, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Remove(path)
}

// StorageUsage reports current retained measured bytes and session count.
func StorageUsage(stateDir string, capBytes int64) (Usage, error) {
	unlockRetention, err := acquireSnapshotLock(stateDir, "retention", unix.LOCK_SH)
	if err != nil {
		return Usage{}, err
	}
	defer unlockRetention()
	root := filepath.Join(stateDir, "snapshots")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return Usage{CapBytes: capBytes, Exact: true}, nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("inspect retained snapshots: %w", err)
	}
	usage := Usage{CapBytes: capBytes, Exact: true}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		value, readErr := readManifest(filepath.Join(root, entry.Name()))
		if readErr != nil {
			usage.Exact = false
			usage.Sessions++
			usage.Warnings = append(usage.Warnings, RetentionWarning{
				SessionID: entry.Name(),
				Error:     "snapshot manifest is missing or unreadable; usage excludes its unknown bytes",
			})
			continue
		}
		usage.Bytes += value.StorageBytes
		if !value.StorageExact {
			usage.Exact = false
		}
		usage.Sessions++
	}
	return usage, nil
}

// copyFile performs the honest full-copy fallback.
func copyFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
