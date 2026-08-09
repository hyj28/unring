package localrollback

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquireSnapshotLock(stateDir, sessionID string, operation int) (func(), error) {
	lockDirectory := filepath.Join(stateDir, "snapshot-locks")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot lock directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(lockDirectory, sessionID+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open snapshot lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock snapshot %s: %w", sessionID, err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
