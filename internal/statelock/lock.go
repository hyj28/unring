// Package statelock coordinates operations on one stored unring session.
package statelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	Shared    = unix.LOCK_SH
	Exclusive = unix.LOCK_EX
)

// Acquire takes a process-wide filesystem lock for one session identity.
func Acquire(stateDir, sessionID string, operation int) (func(), error) {
	return AcquireContext(context.Background(), stateDir, sessionID, operation)
}

// AcquireContext takes a filesystem lock while allowing a caller to cancel a
// wait for another process or phase to release it.
func AcquireContext(ctx context.Context, stateDir, sessionID string, operation int) (func(), error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return nil, fmt.Errorf("lock stored session: invalid session id %q", sessionID)
	}
	lockDirectory := filepath.Join(stateDir, "snapshot-locks")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot lock directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(lockDirectory, sessionID+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open snapshot lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock snapshot %s: %w", sessionID, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock snapshot %s: %w", sessionID, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}
