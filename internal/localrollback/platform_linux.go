//go:build linux

package localrollback

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func newVolumeSnapshotPlatform() VolumeSnapshotPlatform {
	return unavailableSnapshotPlatform{reason: "whole-volume APFS snapshots are unavailable on Linux"}
}

func cloneDirectory(source, destination string) (string, error) {
	return "", syscall.ENOTSUP
}

func cloneOrCopyFile(source, destination string, mode fs.FileMode) (string, bool, error) {
	input, err := os.Open(source)
	if err == nil {
		output, openErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if openErr == nil {
			cloneErr := unix.IoctlFileClone(int(output.Fd()), int(input.Fd()))
			closeErr := output.Close()
			_ = input.Close()
			if cloneErr == nil && closeErr == nil {
				return "reflink", false, nil
			}
			_ = os.Remove(destination)
		} else {
			_ = input.Close()
		}
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
		return stat.Ctim.Nano()
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
