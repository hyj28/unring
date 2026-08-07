// Package localrollback snapshots watched files and restores session changes later.
package localrollback

import (
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
)

const (
	// DefaultRetentionBytes is the default logical-size cap for retained snapshots.
	DefaultRetentionBytes int64 = 5 << 30
	manifestVersion             = 1
)

// Entry is the metadata used to detect a file change and a later restore conflict.
type Entry struct {
	Size       int64  `json:"size"`
	MTime      int64  `json:"mtime_ns"`
	CTime      int64  `json:"ctime_ns"`
	Mode       uint32 `json:"mode"`
	Type       string `json:"type"`
	LinkTarget string `json:"link_target,omitempty"`
}

// CaptureFailure identifies a path that was not protected by the snapshot.
type CaptureFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Change is one created, modified, or deleted path.
type Change struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Before *Entry `json:"before,omitempty"`
	After  *Entry `json:"after,omitempty"`
}

// RestoreRecord is a durable account of one requested restore.
type RestoreRecord struct {
	Path       string    `json:"path"`
	Status     string    `json:"status"`
	Sidecar    string    `json:"snapshot_sidecar,omitempty"`
	Error      string    `json:"error,omitempty"`
	RestoredAt time.Time `json:"restored_at"`
}

// Summary is stored in the session audit record.
type Summary struct {
	Watched       []string         `json:"watched_paths"`
	Uncaptured    []CaptureFailure `json:"uncaptured_paths"`
	Changes       []Change         `json:"changes"`
	Complete      bool             `json:"complete"`
	Error         string           `json:"error,omitempty"`
	Storage       string           `json:"storage"`
	LogicalBytes  int64            `json:"logical_bytes"`
	StorageBytes  int64            `json:"storage_bytes"`
	CopiedBytes   int64            `json:"copied_bytes"`
	RetentionCap  int64            `json:"retention_cap_bytes"`
	Retained      bool             `json:"retained"`
	Evicted       []string         `json:"evicted_sessions,omitempty"`
	RestoreEvents []RestoreRecord  `json:"restores,omitempty"`
}

type rootManifest struct {
	Path       string            `json:"path"`
	Existed    bool              `json:"existed"`
	Snapshot   string            `json:"snapshot"`
	Before     map[string]Entry  `json:"before"`
	Uncaptured map[string]string `json:"uncaptured,omitempty"`
}

type manifest struct {
	Version      int              `json:"version"`
	SessionID    string           `json:"session_id"`
	StartedAt    time.Time        `json:"started_at"`
	EndedAt      time.Time        `json:"ended_at,omitempty"`
	Roots        []rootManifest   `json:"roots"`
	Excluded     []string         `json:"excluded,omitempty"`
	After        map[string]Entry `json:"after,omitempty"`
	Changes      []Change         `json:"changes,omitempty"`
	Complete     bool             `json:"complete"`
	Error        string           `json:"error,omitempty"`
	Storage      string           `json:"storage"`
	LogicalBytes int64            `json:"logical_bytes"`
	StorageBytes int64            `json:"storage_bytes"`
	CopiedBytes  int64            `json:"copied_bytes"`
}

// Session owns the in-progress snapshot for a wrapped child.
type Session struct {
	dir      string
	manifest manifest
	summary  Summary
}

// Usage describes retained snapshot storage.
type Usage struct {
	Bytes    int64
	CapBytes int64
	Sessions int
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

// Start captures watched paths before the child starts. It first attempts the
// platform's recursive fast path, then falls back to per-entry capture so that
// one unreadable path does not erase coverage for the rest of a tree.
func Start(stateDir, sessionID string, watched []string, capBytes int64, now time.Time) (*Session, Summary, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return nil, Summary{}, errors.New("start file snapshot: invalid session id")
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
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, Summary{}, fmt.Errorf("restrict temporary snapshot: %w", err)
	}

	m := manifest{Version: manifestVersion, SessionID: sessionID, StartedAt: now.UTC(), Complete: false}
	absoluteStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, Summary{}, fmt.Errorf("resolve state directory: %w", err)
	}
	m.Excluded = []string{filepath.Clean(absoluteStateDir)}
	var failures []CaptureFailure
	methods := make(map[string]bool)
	for index, root := range roots {
		relSnapshot := filepath.Join("roots", fmt.Sprintf("%06d", index))
		destination := filepath.Join(temporary, relSnapshot)
		rootState, rootFailures, rootMethods, logicalBytes, copiedBytes, captureErr := captureRoot(root, destination, m.Excluded)
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
	m.Storage = storageDescription(methods)
	if err := writeManifest(temporary, m); err != nil {
		return nil, Summary{}, err
	}
	finalDir := filepath.Join(snapshotRoot, sessionID)
	if err := os.Rename(temporary, finalDir); err != nil {
		return nil, Summary{}, fmt.Errorf("publish file snapshot: %w", err)
	}
	cleanup = false
	m.StorageBytes, err = allocatedTree(finalDir)
	if err != nil {
		_ = os.RemoveAll(finalDir)
		return nil, Summary{}, fmt.Errorf("measure file snapshot storage: %w", err)
	}
	if err := writeManifest(finalDir, m); err != nil {
		_ = os.RemoveAll(finalDir)
		return nil, Summary{}, err
	}
	if m.StorageBytes > capBytes {
		_ = os.RemoveAll(finalDir)
		return nil, Summary{}, fmt.Errorf(
			"file snapshot needs %d bytes, exceeding the %d-byte retention cap; child was not started",
			m.StorageBytes, capBytes,
		)
	}

	summary := Summary{
		Watched: roots, Uncaptured: failures, Complete: false,
		Storage: m.Storage, LogicalBytes: m.LogicalBytes, StorageBytes: m.StorageBytes, CopiedBytes: m.CopiedBytes,
		RetentionCap: capBytes, Retained: true,
	}
	session := &Session{dir: finalDir, manifest: m, summary: summary}
	evicted, _, err := EnforceRetention(stateDir, capBytes, sessionID)
	if err != nil {
		_ = os.RemoveAll(finalDir)
		return nil, Summary{}, err
	}
	summary.Evicted = evicted
	session.summary = summary
	return session, summary, nil
}

// Seal scans the watched paths after the child exits and records the diff.
func (s *Session) Seal(now time.Time) Summary {
	after := make(map[string]Entry)
	var scanFailures []CaptureFailure
	for _, root := range s.manifest.Roots {
		entries, failures, err := scanRoot(root.Path, s.manifest.Excluded)
		if err != nil {
			scanFailures = append(scanFailures, CaptureFailure{Path: root.Path, Error: err.Error()})
			continue
		}
		for path, entry := range entries {
			after[path] = entry
		}
		scanFailures = append(scanFailures, failures...)
	}
	before := make(map[string]Entry)
	uncaptured := make(map[string]bool)
	for _, root := range s.manifest.Roots {
		for path, entry := range root.Before {
			before[path] = entry
		}
		for path := range root.Uncaptured {
			uncaptured[path] = true
		}
	}
	for _, failure := range scanFailures {
		uncaptured[failure.Path] = true
	}
	changes := diff(before, after, uncaptured)
	s.manifest.EndedAt = now.UTC()
	s.manifest.After = after
	s.manifest.Changes = changes
	s.manifest.Complete = len(scanFailures) == 0
	if len(scanFailures) > 0 {
		s.manifest.Error = formatFailures("post-session scan could not inspect", scanFailures)
	}
	if err := writeManifest(s.dir, s.manifest); err != nil {
		s.manifest.Complete = false
		s.manifest.Error = joinText(s.manifest.Error, err.Error())
	}
	if storageBytes, err := allocatedTree(s.dir); err != nil {
		s.manifest.Complete = false
		s.manifest.Error = joinText(s.manifest.Error, "measure retained snapshot: "+err.Error())
	} else {
		s.manifest.StorageBytes = storageBytes
		if err := writeManifest(s.dir, s.manifest); err != nil {
			s.manifest.Complete = false
			s.manifest.Error = joinText(s.manifest.Error, err.Error())
		}
	}
	s.summary.Changes = changes
	s.summary.Complete = s.manifest.Complete
	s.summary.Error = s.manifest.Error
	s.summary.StorageBytes = s.manifest.StorageBytes
	return s.summary
}

// Snapshot returns the session's current detached summary.
func (s *Session) Snapshot() Summary {
	data, _ := json.Marshal(s.summary)
	var copied Summary
	_ = json.Unmarshal(data, &copied)
	return copied
}

func captureRoot(root, destination string, excluded []string) (rootManifest, []CaptureFailure, map[string]bool, int64, int64, error) {
	state := rootManifest{Path: root, Before: make(map[string]Entry), Uncaptured: make(map[string]string)}
	methods := make(map[string]bool)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil, methods, 0, 0, nil
	}
	if err != nil {
		failure := CaptureFailure{Path: root, Error: err.Error()}
		state.Uncaptured[root] = err.Error()
		return state, []CaptureFailure{failure}, methods, 0, 0, nil
	}
	state.Existed = true
	before, scanFailures, err := scanRoot(root, excluded)
	if err != nil {
		return state, scanFailures, methods, 0, 0, err
	}
	state.Before = before
	for _, failure := range scanFailures {
		state.Uncaptured[failure.Path] = failure.Error
	}

	if info.IsDir() && !containsExcludedPath(root, excluded) {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return state, scanFailures, methods, 0, 0, err
		}
		if method, err := cloneDirectory(root, destination); err == nil {
			methods[method] = true
			return state, scanFailures, methods, logicalSize(before), 0, nil
		}
		if err := os.RemoveAll(destination); err != nil {
			return state, scanFailures, methods, 0, 0, fmt.Errorf("remove failed recursive clone: %w", err)
		}
	}

	fallbackFailures, fallbackMethods, copiedBytes, err := captureIndividually(root, destination, excluded)
	for _, failure := range fallbackFailures {
		state.Uncaptured[failure.Path] = failure.Error
		delete(state.Before, failure.Path)
	}
	for method := range fallbackMethods {
		methods[method] = true
	}
	failures := mergeFailures(scanFailures, fallbackFailures)
	return state, failures, methods, logicalSize(state.Before), copiedBytes, err
}

func captureIndividually(root, destination string, excluded []string) ([]CaptureFailure, map[string]bool, int64, error) {
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
	entries := make(map[string]Entry)
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
			return entries, []CaptureFailure{{Path: root, Error: err.Error()}}, nil
		}
		entries[root] = entry
		return entries, nil, nil
	}
	var failures []CaptureFailure
	err = filepath.WalkDir(root, func(path string, directoryEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: walkErr.Error()})
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
		if directoryEntry.IsDir() {
			return nil
		}
		info, infoErr := directoryEntry.Info()
		if infoErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: infoErr.Error()})
			return nil
		}
		entry, entryErr := entryFromInfo(path, info)
		if entryErr != nil {
			failures = append(failures, CaptureFailure{Path: path, Error: entryErr.Error()})
			return nil
		}
		entries[path] = entry
		return nil
	})
	return entries, failures, err
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
		Mode: uint32(info.Mode()), Type: "file",
	}
	if info.Mode()&os.ModeSymlink != 0 {
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

func diff(before, after map[string]Entry, uncaptured map[string]bool) []Change {
	changes := make([]Change, 0)
	for path, oldEntry := range before {
		if coveredBy(path, uncaptured) {
			continue
		}
		newEntry, exists := after[path]
		if !exists {
			beforeCopy := oldEntry
			changes = append(changes, Change{Kind: "deleted", Path: path, Before: &beforeCopy})
			continue
		}
		if oldEntry != newEntry {
			beforeCopy, afterCopy := oldEntry, newEntry
			changes = append(changes, Change{Kind: "modified", Path: path, Before: &beforeCopy, After: &afterCopy})
		}
	}
	for path, newEntry := range after {
		if _, existed := before[path]; existed || coveredBy(path, uncaptured) {
			continue
		}
		afterCopy := newEntry
		changes = append(changes, Change{Kind: "created", Path: path, After: &afterCopy})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
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
			if candidate == parent || strings.HasPrefix(candidate, parent+string(os.PathSeparator)) {
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
	byPath := make(map[string]string)
	for _, group := range groups {
		for _, failure := range group {
			byPath[failure.Path] = failure.Error
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]CaptureFailure, 0, len(paths))
	for _, path := range paths {
		out = append(out, CaptureFailure{Path: path, Error: byPath[path]})
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

// EnforceRetention evicts oldest completed snapshots until the logical-size cap is met.
// The active session is kept; callers reject it separately if it alone exceeds the cap.
func EnforceRetention(stateDir string, capBytes int64, activeSession string) ([]string, Usage, error) {
	root := filepath.Join(stateDir, "snapshots")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Usage{CapBytes: capBytes}, nil
	}
	if err != nil {
		return nil, Usage{}, fmt.Errorf("inspect retained snapshots: %w", err)
	}
	type retained struct {
		id      string
		started time.Time
		bytes   int64
	}
	var snapshots []retained
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		value, readErr := readManifest(filepath.Join(root, entry.Name()))
		if readErr != nil {
			return nil, Usage{}, fmt.Errorf("read retained snapshot %s: %w", entry.Name(), readErr)
		}
		bytes := value.StorageBytes
		if bytes == 0 {
			bytes = value.LogicalBytes
		}
		snapshots = append(snapshots, retained{id: entry.Name(), started: value.StartedAt, bytes: bytes})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].started.Equal(snapshots[j].started) {
			return snapshots[i].id < snapshots[j].id
		}
		return snapshots[i].started.Before(snapshots[j].started)
	})
	var total int64
	for _, snapshot := range snapshots {
		total += snapshot.bytes
	}
	var evicted []string
	for _, snapshot := range snapshots {
		if total <= capBytes {
			break
		}
		if snapshot.id == activeSession {
			continue
		}
		path := filepath.Join(root, snapshot.id)
		if err := os.RemoveAll(path); err != nil {
			return evicted, Usage{}, fmt.Errorf("evict snapshot %s: %w", snapshot.id, err)
		}
		total -= snapshot.bytes
		evicted = append(evicted, snapshot.id)
	}
	return evicted, Usage{Bytes: total, CapBytes: capBytes, Sessions: len(snapshots) - len(evicted)}, nil
}

// StorageUsage reports current retained logical bytes and session count.
func StorageUsage(stateDir string, capBytes int64) (Usage, error) {
	root := filepath.Join(stateDir, "snapshots")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return Usage{CapBytes: capBytes}, nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("inspect retained snapshots: %w", err)
	}
	usage := Usage{CapBytes: capBytes}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		value, readErr := readManifest(filepath.Join(root, entry.Name()))
		if readErr != nil {
			return Usage{}, fmt.Errorf("read retained snapshot %s: %w", entry.Name(), readErr)
		}
		bytes := value.StorageBytes
		if bytes == 0 {
			bytes = value.LogicalBytes
		}
		usage.Bytes += bytes
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
