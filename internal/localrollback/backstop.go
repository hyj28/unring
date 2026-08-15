package localrollback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Volume identifies the mounted APFS volume that contains a path.
type Volume struct {
	ID         string `json:"id"`
	MountPoint string `json:"mount_point"`
	Filesystem string `json:"filesystem"`
}

// VolumeSnapshot names one APFS snapshot on one mounted volume.
type VolumeSnapshot struct {
	Name       string `json:"name"`
	VolumeID   string `json:"volume_id"`
	MountPoint string `json:"mount_point"`
}

// Backstop describes the whole-volume protection attempted for a session.
type Backstop struct {
	Checked   bool             `json:"checked"`
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty"`
	Snapshots []VolumeSnapshot `json:"snapshots,omitempty"`
	Excluded  []CaptureFailure `json:"excluded_paths,omitempty"`
}

// SnapshotPresence reports whether a recorded APFS snapshot is still present.
type SnapshotPresence struct {
	Snapshot VolumeSnapshot
	Present  bool
	Error    string
}

// VolumeSnapshotPlatform is the platform boundary for whole-volume snapshots.
// Implementations must not require callers to know about tmutil or mount_apfs.
type VolumeSnapshotPlatform interface {
	Supported() (bool, string)
	VolumeForPath(path string) (Volume, error)
	IsExcluded(path string) (bool, error)
	IsExcludedBatch(ctx context.Context, paths []string) ([]bool, error)
	ListSnapshots(volume Volume) ([]string, error)
	CreateSnapshots() (string, error)
	MountSnapshot(snapshot VolumeSnapshot, mountPoint string) error
	UnmountSnapshot(mountPoint string) error
}

var snapshotPlatformState = struct {
	sync.RWMutex
	platform VolumeSnapshotPlatform
}{}

func currentSnapshotPlatform() VolumeSnapshotPlatform {
	snapshotPlatformState.RLock()
	platform := snapshotPlatformState.platform
	snapshotPlatformState.RUnlock()
	if platform != nil {
		return platform
	}
	if os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") != "" {
		return unavailableSnapshotPlatform{reason: "whole-volume backstop disabled by the test environment"}
	}
	return newVolumeSnapshotPlatform()
}

// SetVolumeSnapshotPlatformForTest substitutes the platform boundary and
// returns a function that restores the previous implementation. Tests using
// it must not run concurrently with snapshot operations.
func SetVolumeSnapshotPlatformForTest(platform VolumeSnapshotPlatform) func() {
	snapshotPlatformState.Lock()
	previous := snapshotPlatformState.platform
	snapshotPlatformState.platform = platform
	snapshotPlatformState.Unlock()
	return func() {
		snapshotPlatformState.Lock()
		snapshotPlatformState.platform = previous
		snapshotPlatformState.Unlock()
	}
}

type unavailableSnapshotPlatform struct {
	reason string
}

func (platform unavailableSnapshotPlatform) Supported() (bool, string) {
	return false, platform.reason
}

func (unavailableSnapshotPlatform) VolumeForPath(string) (Volume, error) {
	return Volume{}, errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) IsExcluded(string) (bool, error) {
	return false, errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) IsExcludedBatch(context.Context, []string) ([]bool, error) {
	return nil, errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) ListSnapshots(Volume) ([]string, error) {
	return nil, errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) CreateSnapshots() (string, error) {
	return "", errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) MountSnapshot(VolumeSnapshot, string) error {
	return errors.New("volume snapshots are unavailable")
}

func (unavailableSnapshotPlatform) UnmountSnapshot(string) error {
	return errors.New("volume snapshots are unavailable")
}

func takeBackstop(paths []string, platform VolumeSnapshotPlatform) Backstop {
	if unavailable, ok := platform.(unavailableSnapshotPlatform); ok &&
		unavailable.reason == "whole-volume backstop disabled by the test environment" {
		return Backstop{}
	}
	backstop := Backstop{Checked: true}
	if supported, reason := platform.Supported(); !supported {
		backstop.Reason = reason
		return backstop
	}

	volumes := make(map[string]Volume)
	for _, path := range uniqueCleanPaths(paths) {
		excluded, err := platform.IsExcluded(path)
		if err != nil {
			backstop.Excluded = append(backstop.Excluded, CaptureFailure{
				Path: path, Error: "could not verify Time Machine inclusion: " + err.Error(),
			})
			continue
		}
		if excluded {
			backstop.Excluded = append(backstop.Excluded, CaptureFailure{
				Path: path, Error: "excluded from the Time Machine backup",
			})
			continue
		}
		volume, err := platform.VolumeForPath(path)
		if err != nil {
			backstop.Excluded = append(backstop.Excluded, CaptureFailure{
				Path: path, Error: "could not identify its volume: " + err.Error(),
			})
			continue
		}
		if !strings.EqualFold(volume.Filesystem, "apfs") {
			backstop.Excluded = append(backstop.Excluded, CaptureFailure{
				Path: path, Error: fmt.Sprintf("volume filesystem is %s, not APFS", volume.Filesystem),
			})
			continue
		}
		volumes[volume.ID] = volume
	}
	backstop.Excluded = mergeFailures(backstop.Excluded)
	if len(volumes) == 0 {
		backstop.Reason = "no included APFS volume was found for the scanned or watched paths"
		return backstop
	}

	before := make(map[string]map[string]bool, len(volumes))
	for id, volume := range volumes {
		names, err := platform.ListSnapshots(volume)
		if err != nil {
			backstop.Reason = "could not list local Time Machine snapshots before capture: " + err.Error()
			return backstop
		}
		before[id] = stringSet(names)
	}
	createdName, err := platform.CreateSnapshots()
	if err != nil {
		backstop.Reason = "Time Machine did not create a local snapshot: " + err.Error()
		return backstop
	}

	volumeIDs := make([]string, 0, len(volumes))
	for id := range volumes {
		volumeIDs = append(volumeIDs, id)
	}
	sort.Strings(volumeIDs)
	for _, id := range volumeIDs {
		volume := volumes[id]
		names, err := platform.ListSnapshots(volume)
		if err != nil {
			backstop.Reason = "local snapshot was requested, but it could not be identified on " + volume.MountPoint + ": " + err.Error()
			backstop.Snapshots = nil
			return backstop
		}
		name := newlyCreatedSnapshot(before[id], names, createdName)
		if name == "" {
			backstop.Excluded = append(backstop.Excluded, CaptureFailure{
				Path:  volume.MountPoint,
				Error: "tmutil reported success but no new local snapshot appeared on this volume",
			})
			continue
		}
		backstop.Snapshots = append(backstop.Snapshots, VolumeSnapshot{
			Name: name, VolumeID: volume.ID, MountPoint: volume.MountPoint,
		})
	}
	backstop.Available = len(backstop.Snapshots) > 0
	if !backstop.Available && backstop.Reason == "" {
		backstop.Reason = "tmutil did not create a local snapshot on any relevant APFS volume"
	}
	return backstop
}

func uniqueCleanPaths(paths []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func newlyCreatedSnapshot(before map[string]bool, after []string, reported string) string {
	if reported != "" {
		for _, name := range after {
			if name == reported {
				return name
			}
		}
	}
	var added []string
	for _, name := range after {
		if !before[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	if len(added) == 0 {
		return ""
	}
	return added[len(added)-1]
}

func snapshotForPath(platform VolumeSnapshotPlatform, backstop Backstop, path string) (VolumeSnapshot, bool) {
	for _, failure := range backstop.Excluded {
		if path == failure.Path || strings.HasPrefix(path, failure.Path+string(os.PathSeparator)) {
			return VolumeSnapshot{}, false
		}
	}
	volume, err := platform.VolumeForPath(path)
	if err != nil {
		return VolumeSnapshot{}, false
	}
	for _, snapshot := range backstop.Snapshots {
		if snapshot.VolumeID == volume.ID {
			return snapshot, true
		}
	}
	return VolumeSnapshot{}, false
}

// InspectBackstop checks whether every snapshot recorded for a session is
// still present on its volume. An unavailable backstop produces no entries.
func InspectBackstop(backstop Backstop) []SnapshotPresence {
	platform := currentSnapshotPlatform()
	result := make([]SnapshotPresence, 0, len(backstop.Snapshots))
	for _, snapshot := range backstop.Snapshots {
		volume := Volume{ID: snapshot.VolumeID, MountPoint: snapshot.MountPoint, Filesystem: "apfs"}
		names, err := platform.ListSnapshots(volume)
		presence := SnapshotPresence{Snapshot: snapshot}
		if err != nil {
			presence.Error = err.Error()
		} else {
			for _, name := range names {
				if name == snapshot.Name {
					presence.Present = true
					break
				}
			}
		}
		result = append(result, presence)
	}
	return result
}
