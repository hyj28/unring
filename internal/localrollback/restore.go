package localrollback

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RestoreResult describes one selected path.
type RestoreResult struct {
	Path    string
	Status  string
	Sidecar string
	Err     error
}

// LoadSummary loads the durable file summary for a retained snapshot.
func LoadSummary(stateDir, sessionID string) (Summary, error) {
	resolved, err := resolveSessionID(stateDir, sessionID)
	if err != nil {
		return Summary{}, err
	}
	unlock, err := acquireSnapshotLock(stateDir, resolved, unix.LOCK_SH)
	if err != nil {
		return Summary{}, err
	}
	defer unlock()
	value, err := readManifest(filepath.Join(stateDir, "snapshots", resolved))
	if err != nil {
		return Summary{}, err
	}
	return summaryFromManifest(value, true), nil
}

// Restore applies selected file changes. Paths are exact absolute paths or
// paths relative to the caller's current working directory.
func Restore(stateDir, sessionID string, selections []string, force bool) ([]RestoreResult, error) {
	resolved, err := resolveSessionID(stateDir, sessionID)
	if err != nil {
		return nil, err
	}
	unlock, err := acquireSnapshotLock(stateDir, resolved, unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock()
	value, err := readManifest(filepath.Join(stateDir, "snapshots", resolved))
	if err != nil {
		return nil, err
	}
	if value.EndedAt.IsZero() {
		return nil, fmt.Errorf("snapshot %s is incomplete because capture is still in progress and cannot be restored", sessionID)
	}
	selected, err := selectChanges(value.Changes, selections)
	if err != nil {
		return nil, err
	}
	selected = orderRestoreChanges(selected)
	rootDirectory := filepath.Join(stateDir, "snapshots", value.SessionID)
	results := make([]RestoreResult, 0, len(selected))
	for _, change := range selected {
		if change.UnrestorableReason != "" {
			results = append(results, RestoreResult{
				Path: change.Path, Status: "unavailable",
				Err: fmt.Errorf("path was outside snapshot coverage: %s", change.UnrestorableReason),
			})
			continue
		}
		result := restoreOne(rootDirectory, value, change, force)
		results = append(results, result)
	}
	return results, nil
}

func orderRestoreChanges(changes []Change) []Change {
	ordered := append([]Change(nil), changes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := restoreRank(ordered[i]), restoreRank(ordered[j])
		if left != right {
			return left < right
		}
		leftDepth := strings.Count(filepath.Clean(ordered[i].Path), string(os.PathSeparator))
		rightDepth := strings.Count(filepath.Clean(ordered[j].Path), string(os.PathSeparator))
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return ordered[i].Path < ordered[j].Path
	})
	return ordered
}

func restoreRank(change Change) int {
	if change.Kind == "created" {
		if change.After != nil && change.After.Type == "directory" {
			return 3
		}
		return 2
	}
	if change.Before != nil && change.Before.Type == "directory" {
		return 1
	}
	return 0
}

func resolveSessionID(stateDir, sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return "", errors.New("load file snapshot: invalid session id")
	}
	root := filepath.Join(stateDir, "snapshots")
	exact := filepath.Join(root, sessionID)
	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		return sessionID, nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("file snapshot %q not found (it may have been evicted)", sessionID)
	}
	if err != nil {
		return "", err
	}
	match := ""
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasPrefix(entry.Name(), sessionID) {
			if match != "" {
				return "", fmt.Errorf("file snapshot prefix %q is ambiguous", sessionID)
			}
			match = entry.Name()
		}
	}
	if match == "" {
		return "", fmt.Errorf("file snapshot %q not found (it may have been evicted)", sessionID)
	}
	return match, nil
}

func summaryFromManifest(value manifest, retained bool) Summary {
	var watched []string
	for _, root := range value.Roots {
		watched = append(watched, root.Path)
	}
	return Summary{
		Watched: watched, Uncaptured: manifestFailures(value), Changes: value.Changes,
		Complete: value.Complete, Error: value.Error, Storage: value.Storage,
		LogicalBytes: value.LogicalBytes, StorageBytes: value.StorageBytes,
		StorageExact: value.StorageExact, CopiedBytes: value.CopiedBytes,
		RetentionCap: value.RetentionCap, Retained: retained,
	}
}

func selectChanges(changes []Change, selections []string) ([]Change, error) {
	if len(selections) == 0 {
		return nil, nil
	}
	byAbsolute := make(map[string]Change, len(changes))
	for _, change := range changes {
		byAbsolute[filepath.Clean(change.Path)] = change
	}
	selected := make([]Change, 0, len(selections))
	seen := make(map[string]bool)
	for _, selection := range selections {
		originalSelection := selection
		absolute := selection
		if !filepath.IsAbs(absolute) {
			var err error
			absolute, err = filepath.Abs(absolute)
			if err != nil {
				return nil, err
			}
		}
		change, exists := byAbsolute[filepath.Clean(absolute)]
		if !exists && !filepath.IsAbs(originalSelection) {
			cleanSelection := filepath.Clean(originalSelection)
			for _, candidate := range changes {
				if candidate.Path == cleanSelection || strings.HasSuffix(candidate.Path, string(os.PathSeparator)+cleanSelection) {
					if exists {
						return nil, fmt.Errorf("%q matches more than one changed path; use an absolute path", originalSelection)
					}
					change, exists = candidate, true
				}
			}
		}
		if !exists {
			return nil, fmt.Errorf("%q is not a changed path in this session", selection)
		}
		if !seen[change.Path] {
			selected = append(selected, change)
			seen[change.Path] = true
		}
	}
	return selected, nil
}

func restoreOne(snapshotRoot string, value manifest, change Change, force bool) RestoreResult {
	result := RestoreResult{Path: change.Path}
	if err := verifyRestoreRoot(value, change.Path); err != nil {
		result.Status, result.Err = "refused", err
		return result
	}
	current, exists, err := currentEntry(change.Path)
	if err != nil {
		result.Status, result.Err = "error", err
		return result
	}
	alreadyRestored, err := pathMatchesBefore(snapshotRoot, value, change, current, exists)
	if err != nil {
		result.Status, result.Err = "error", err
		return result
	}
	if alreadyRestored {
		result.Status = "already-restored"
		return result
	}
	conflict := !matchesExpected(current, exists, change.After)
	if conflict && !force {
		result.Status = "refused"
		if change.Before != nil {
			snapshotPath, err := snapshotPathFor(value, snapshotRoot, change.Path)
			if err != nil {
				result.Err = err
				return result
			}
			sidecar := availableSidecar(change.Path, value.SessionID)
			if err := restoreSnapshotPath(snapshotPath, sidecar, change.Before); err != nil {
				result.Err = fmt.Errorf("write snapshot version alongside conflict: %w", err)
				return result
			}
			result.Sidecar = sidecar
		}
		return result
	}

	switch change.Kind {
	case "created":
		if exists {
			if err := os.Remove(change.Path); err != nil {
				result.Status, result.Err = "error", err
				return result
			}
		}
		result.Status = "restored"
	case "modified", "deleted":
		snapshotPath, err := snapshotPathFor(value, snapshotRoot, change.Path)
		if err != nil {
			result.Status, result.Err = "error", err
			return result
		}
		if err := restoreSnapshotObject(snapshotPath, change.Path, change.Before, value); err != nil {
			result.Status, result.Err = "error", err
			return result
		}
		result.Status = "restored"
	default:
		result.Status = "error"
		result.Err = fmt.Errorf("unsupported change kind %q", change.Kind)
	}
	return result
}

func restoreSnapshotObject(snapshotPath, destination string, metadata *Entry, value manifest) error {
	if metadata == nil {
		return errors.New("snapshot metadata is absent")
	}
	createdParents, err := ensureParentDirectories(value, destination)
	if err != nil {
		return err
	}
	var restoreErr error
	if metadata.Type == "directory" {
		if err := os.Mkdir(destination, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			restoreErr = err
		} else {
			restoreErr = applyMetadata(destination, *metadata)
		}
	} else {
		restoreErr = restoreSnapshotAtomically(snapshotPath, destination, metadata)
	}
	return errors.Join(restoreErr, applyCreatedParentMetadata(createdParents))
}

func applyCreatedParentMetadata(created []restoredParent) error {
	var result error
	for index := len(created) - 1; index >= 0; index-- {
		result = errors.Join(result, applyMetadata(created[index].path, created[index].entry))
	}
	return result
}

func restoreSnapshotAtomically(snapshotPath, destination string, metadata *Entry) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".unring-restore-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := restoreSnapshotPath(snapshotPath, temporaryPath, metadata); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

type restoredParent struct {
	path  string
	entry Entry
}

func ensureParentDirectories(value manifest, destination string) ([]restoredParent, error) {
	root, found := manifestRootFor(value, destination)
	if !found {
		return nil, fmt.Errorf("snapshot has no root for %s", destination)
	}
	parent := filepath.Dir(destination)
	if rootEntry, exists := root.Before[root.Path]; root.Path == destination && exists && rootEntry.Type != "directory" {
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("restore parent %s is unavailable", parent)
		}
		return nil, nil
	}
	if parent != root.Path && !strings.HasPrefix(parent, root.Path+string(os.PathSeparator)) {
		return nil, fmt.Errorf("restore parent escaped watched root: %s", parent)
	}
	var paths []string
	for path := parent; path == root.Path || strings.HasPrefix(path, root.Path+string(os.PathSeparator)); path = filepath.Dir(path) {
		paths = append(paths, path)
		if path == root.Path {
			break
		}
	}
	var created []restoredParent
	for index := len(paths) - 1; index >= 0; index-- {
		path := paths[index]
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return created, errors.Join(
					fmt.Errorf("restore parent %s is not a directory", path),
					applyCreatedParentMetadata(created),
				)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return created, errors.Join(err, applyCreatedParentMetadata(created))
		}
		entry, exists := root.Before[path]
		if !exists || entry.Type != "directory" {
			return created, errors.Join(
				fmt.Errorf("snapshot has no directory metadata for missing parent %s", path),
				applyCreatedParentMetadata(created),
			)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return created, errors.Join(err, applyCreatedParentMetadata(created))
		}
		created = append(created, restoredParent{path: path, entry: entry})
	}
	return created, nil
}

func manifestRootFor(value manifest, path string) (rootManifest, bool) {
	var matched rootManifest
	found := false
	for _, root := range value.Roots {
		if path == root.Path || strings.HasPrefix(path, root.Path+string(os.PathSeparator)) {
			if !found || len(root.Path) > len(matched.Path) {
				matched, found = root, true
			}
		}
	}
	return matched, found
}

func verifyRestoreRoot(value manifest, path string) error {
	root, found := manifestRootFor(value, path)
	if !found {
		return fmt.Errorf("snapshot has no watched root for %s", path)
	}
	resolved, err := resolveAllowMissing(root.Path)
	if err != nil {
		return fmt.Errorf("resolve watched root %s before restore: %w", root.Path, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(root.Source) {
		return fmt.Errorf("watched root %s now resolves to %s instead of snapshotted target %s", root.Path, resolved, root.Source)
	}
	return nil
}

func resolveAllowMissing(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			parts := append([]string{resolved}, missing...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

func currentEntry(path string) (Entry, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	entry, err := entryFromInfo(path, info)
	return entry, true, err
}

func matchesExpected(current Entry, exists bool, expected *Entry) bool {
	if expected == nil {
		return !exists
	}
	return exists && !entriesDiffer(current, *expected)
}

func pathMatchesBefore(
	snapshotRoot string,
	value manifest,
	change Change,
	current Entry,
	exists bool,
) (bool, error) {
	if change.Before == nil {
		return !exists, nil
	}
	if !exists || current.Type != change.Before.Type {
		return false, nil
	}
	if current.Mode != change.Before.Mode {
		return false, nil
	}
	switch change.Before.Type {
	case "directory":
		return true, nil
	case "symlink":
		return current.LinkTarget == change.Before.LinkTarget, nil
	case "file":
		if current.Size != change.Before.Size || current.MTime != change.Before.MTime {
			return false, nil
		}
		snapshotPath, err := snapshotPathFor(value, snapshotRoot, change.Path)
		if err != nil {
			return false, err
		}
		return filesEqual(snapshotPath, change.Path)
	default:
		return false, nil
	}
}

func filesEqual(leftPath, rightPath string) (bool, error) {
	left, err := os.ReadFile(leftPath)
	if err != nil {
		return false, err
	}
	right, err := os.ReadFile(rightPath)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func snapshotPathFor(value manifest, snapshotRoot, original string) (string, error) {
	root, found := manifestRootFor(value, original)
	if !found {
		return "", fmt.Errorf("snapshot has no root for %s", original)
	}
	relative, err := filepath.Rel(root.Path, original)
	if err != nil {
		return "", err
	}
	path := filepath.Join(snapshotRoot, root.Snapshot)
	if relative != "." {
		path = filepath.Join(path, relative)
	}
	cleanRoot := filepath.Clean(filepath.Join(snapshotRoot, root.Snapshot))
	cleanPath := filepath.Clean(path)
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator)) {
		return "", errors.New("snapshot path escaped its root")
	}
	return cleanPath, nil
}

func restoreSnapshotPath(snapshotPath, destination string, metadata *Entry) error {
	if metadata == nil {
		return errors.New("snapshot metadata is absent")
	}
	if metadata.Type == "directory" {
		if err := os.Mkdir(destination, 0o700); err != nil {
			return err
		}
		return applyMetadata(destination, *metadata)
	}
	if metadata.Type == "symlink" {
		target, err := os.Readlink(snapshotPath)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if metadata.Type != "file" {
		return fmt.Errorf("cannot restore unsupported snapshot type %q", metadata.Type)
	}
	method, _, err := cloneOrCopyFile(snapshotPath, destination, restorableMode(metadata.Mode))
	_ = method
	if err != nil {
		return err
	}
	return applyMetadata(destination, *metadata)
}

func restorableMode(mode uint32) fs.FileMode {
	value := fs.FileMode(mode)
	return value & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func applyMetadata(path string, metadata Entry) error {
	if metadata.Type != "symlink" {
		if err := os.Chmod(path, restorableMode(metadata.Mode)); err != nil {
			return err
		}
		mtime := time.Unix(0, metadata.MTime)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			return err
		}
	}
	return nil
}

func availableSidecar(path, sessionID string) string {
	shortID := sessionID
	if len(shortID) > 20 {
		shortID = shortID[:20]
	}
	base := path + ".unring-" + shortID + ".snapshot"
	for index := 0; ; index++ {
		candidate := base
		if index > 0 {
			candidate = fmt.Sprintf("%s.%d", base, index)
		}
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}
