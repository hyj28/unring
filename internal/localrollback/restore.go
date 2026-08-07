package localrollback

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	value, err := loadSessionManifest(stateDir, sessionID)
	if err != nil {
		return Summary{}, err
	}
	return summaryFromManifest(value, true), nil
}

// Restore applies selected file changes. Paths are exact absolute paths or
// paths relative to the caller's current working directory.
func Restore(stateDir, sessionID string, selections []string, force bool) ([]RestoreResult, error) {
	value, err := loadSessionManifest(stateDir, sessionID)
	if err != nil {
		return nil, err
	}
	if !value.Complete {
		return nil, fmt.Errorf("snapshot %s has an incomplete post-session scan: %s", sessionID, value.Error)
	}
	selected, err := selectChanges(value.Changes, selections)
	if err != nil {
		return nil, err
	}
	rootDirectory := filepath.Join(stateDir, "snapshots", value.SessionID)
	results := make([]RestoreResult, 0, len(selected))
	for _, change := range selected {
		result := restoreOne(rootDirectory, value, change, force)
		results = append(results, result)
	}
	return results, nil
}

func loadSessionManifest(stateDir, sessionID string) (manifest, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return manifest{}, errors.New("load file snapshot: invalid session id")
	}
	root := filepath.Join(stateDir, "snapshots")
	exact := filepath.Join(root, sessionID)
	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		return readManifest(exact)
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{}, fmt.Errorf("file snapshot %q not found (it may have been evicted)", sessionID)
	}
	if err != nil {
		return manifest{}, err
	}
	match := ""
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasPrefix(entry.Name(), sessionID) {
			if match != "" {
				return manifest{}, fmt.Errorf("file snapshot prefix %q is ambiguous", sessionID)
			}
			match = entry.Name()
		}
	}
	if match == "" {
		return manifest{}, fmt.Errorf("file snapshot %q not found (it may have been evicted)", sessionID)
	}
	return readManifest(filepath.Join(root, match))
}

func summaryFromManifest(value manifest, retained bool) Summary {
	var watched []string
	var failures []CaptureFailure
	for _, root := range value.Roots {
		watched = append(watched, root.Path)
		for path, message := range root.Uncaptured {
			failures = append(failures, CaptureFailure{Path: path, Error: message})
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Path < failures[j].Path })
	return Summary{
		Watched: watched, Uncaptured: failures, Changes: value.Changes,
		Complete: value.Complete, Error: value.Error, Storage: value.Storage,
		LogicalBytes: value.LogicalBytes, StorageBytes: value.StorageBytes,
		CopiedBytes: value.CopiedBytes, Retained: retained,
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
	current, exists, err := currentEntry(change.Path)
	if err != nil {
		result.Status, result.Err = "error", err
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
		if err := restoreSnapshotAtomically(snapshotPath, change.Path, change.Before); err != nil {
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

func restoreSnapshotAtomically(snapshotPath, destination string, metadata *Entry) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
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
	return exists && current == *expected
}

func snapshotPathFor(value manifest, snapshotRoot, original string) (string, error) {
	for _, root := range value.Roots {
		if original != root.Path && !strings.HasPrefix(original, root.Path+string(os.PathSeparator)) {
			continue
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
	return "", fmt.Errorf("snapshot has no root for %s", original)
}

func restoreSnapshotPath(snapshotPath, destination string, metadata *Entry) error {
	if metadata == nil {
		return errors.New("snapshot metadata is absent")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
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
	method, _, err := cloneOrCopyFile(snapshotPath, destination, fs.FileMode(metadata.Mode).Perm())
	_ = method
	if err != nil {
		return err
	}
	if err := os.Chmod(destination, fs.FileMode(metadata.Mode).Perm()); err != nil {
		return err
	}
	mtime := time.Unix(0, metadata.MTime)
	return os.Chtimes(destination, mtime, mtime)
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
