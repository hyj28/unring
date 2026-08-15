//go:build darwin

package localrollback

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type commandOutputRunner interface {
	Run(name string, args ...string) ([]byte, error)
	RunContext(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execOutputRunner struct{}

func (execOutputRunner) Run(name string, args ...string) ([]byte, error) {
	return execOutputRunner{}.RunContext(context.Background(), name, args...)
}

func (execOutputRunner) RunContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return output, err
		}
		return output, fmt.Errorf("%w: %s", err, message)
	}
	return output, nil
}

type darwinVolumeSnapshotPlatform struct {
	runner commandOutputRunner
}

func newVolumeSnapshotPlatform() VolumeSnapshotPlatform {
	return &darwinVolumeSnapshotPlatform{runner: execOutputRunner{}}
}

func (*darwinVolumeSnapshotPlatform) Supported() (bool, string) {
	return true, ""
}

func (*darwinVolumeSnapshotPlatform) VolumeForPath(path string) (Volume, error) {
	path, err := nearestExistingPath(path)
	if err != nil {
		return Volume{}, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return Volume{}, err
	}
	return Volume{
		ID:         int8String(stat.Mntfromname[:]),
		MountPoint: int8String(stat.Mntonname[:]),
		Filesystem: int8String(stat.Fstypename[:]),
	}, nil
}

func (platform *darwinVolumeSnapshotPlatform) IsExcluded(path string) (bool, error) {
	results, err := platform.IsExcludedBatch(context.Background(), []string{path})
	if err != nil {
		return false, err
	}
	return results[0], nil

}

func (platform *darwinVolumeSnapshotPlatform) IsExcludedBatch(ctx context.Context, paths []string) ([]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	for _, path := range paths {
		if strings.ContainsAny(path, "\r\n") {
			return nil, fmt.Errorf("cannot safely parse tmutil isexcluded output for path containing a line break: %q", path)
		}
	}
	arguments := append([]string{"isexcluded"}, paths...)
	output, err := platform.runner.RunContext(ctx, "/usr/bin/tmutil", arguments...)
	if err != nil {
		return nil, err
	}
	return parseExcludedBatch(paths, output)
}

func parseExcludedBatch(paths []string, output []byte) ([]bool, error) {
	trimmed := strings.TrimSpace(string(output))
	lines := []string(nil)
	if trimmed != "" {
		lines = strings.Split(trimmed, "\n")
	}
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("unexpected tmutil isexcluded output: got %d result lines for %d paths", len(lines), len(paths))
	}
	results := make([]bool, len(paths))
	for index, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		var excluded bool
		var reported string
		switch {
		case strings.HasPrefix(line, "[Excluded]"):
			excluded = true
			reported = strings.TrimSpace(strings.TrimPrefix(line, "[Excluded]"))
		case strings.HasPrefix(line, "[Included]"):
			reported = strings.TrimSpace(strings.TrimPrefix(line, "[Included]"))
		default:
			return nil, fmt.Errorf("unexpected tmutil isexcluded output line %d: %q", index+1, line)
		}
		if reported != paths[index] {
			return nil, fmt.Errorf("unexpected tmutil isexcluded output line %d: reported path %q for requested path %q", index+1, reported, paths[index])
		}
		results[index] = excluded
	}
	return results, nil
}

func (platform *darwinVolumeSnapshotPlatform) ListSnapshots(volume Volume) ([]string, error) {
	output, err := platform.runner.Run("/usr/bin/tmutil", "listlocalsnapshots", volume.MountPoint)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		name := strings.TrimSpace(string(line))
		if strings.HasPrefix(name, "com.apple.TimeMachine.") && strings.HasSuffix(name, ".local") {
			names = append(names, name)
		}
	}
	return names, nil
}

var localSnapshotDate = regexp.MustCompile(`(?m)(\d{4}-\d{2}-\d{2}-\d{6})`)

func (platform *darwinVolumeSnapshotPlatform) CreateSnapshots() (string, error) {
	output, err := platform.runner.Run("/usr/bin/tmutil", "localsnapshot")
	if err != nil {
		return "", err
	}
	match := localSnapshotDate.FindSubmatch(output)
	if len(match) != 2 {
		return "", nil
	}
	return "com.apple.TimeMachine." + string(match[1]) + ".local", nil
}

func (platform *darwinVolumeSnapshotPlatform) MountSnapshot(snapshot VolumeSnapshot, mountPoint string) error {
	_, err := platform.runner.Run(
		"/usr/bin/sudo", "/sbin/mount_apfs", "-o", "rdonly", "-s", snapshot.Name,
		snapshot.MountPoint, mountPoint,
	)
	return err
}

func (platform *darwinVolumeSnapshotPlatform) UnmountSnapshot(mountPoint string) error {
	_, err := platform.runner.Run("/usr/bin/sudo", "/sbin/umount", mountPoint)
	return err
}

func nearestExistingPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", os.ErrNotExist
		}
		path = parent
	}
}

func int8String(value []byte) string {
	result := make([]byte, 0, len(value))
	for _, character := range value {
		if character == 0 {
			break
		}
		result = append(result, character)
	}
	return string(result)
}

func cloneDirectory(source, destination string) (string, error) {
	return "clonefile", unix.Clonefile(source, destination, 0)
}

func cloneOrCopyFile(source, destination string, mode fs.FileMode) (string, bool, error) {
	if err := unix.Clonefile(source, destination, 0); err == nil {
		return "clonefile", false, nil
	}
	if err := copyFile(source, destination, mode); err != nil {
		return "", false, err
	}
	return "full-copy", true, nil
}

func readable(path string) error {
	return unix.Access(path, unix.R_OK)
}

func changeTime(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ctimespec.Nano()
	}
	return 0
}

func linkCount(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}

func filesystemAvailableBytes(path string) (int64, bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), true, nil
}

func allocatedSize(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Blocks * 512
	}
	return info.Size()
}
