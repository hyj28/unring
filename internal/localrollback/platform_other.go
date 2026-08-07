//go:build !darwin && !linux

package localrollback

import (
	"io/fs"
	"os"
	"syscall"
)

func cloneDirectory(source, destination string) (string, error) {
	return "", syscall.ENOTSUP
}

func cloneOrCopyFile(source, destination string, mode fs.FileMode) (string, bool, error) {
	if err := copyFile(source, destination, mode); err != nil {
		return "", false, err
	}
	return "full-copy", true, nil
}

func readable(path string) error {
	file, err := os.Open(path)
	if err == nil {
		err = file.Close()
	}
	return err
}

func changeTime(fs.FileInfo) int64 { return 0 }

func allocatedSize(info fs.FileInfo) int64 { return info.Size() }
