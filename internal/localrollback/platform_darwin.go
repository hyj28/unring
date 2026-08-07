//go:build darwin

package localrollback

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

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
