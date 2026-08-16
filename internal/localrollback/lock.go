package localrollback

import (
	"context"

	"github.com/hyj28/unring/internal/statelock"
)

func acquireSnapshotLock(stateDir, sessionID string, operation int) (func(), error) {
	return statelock.Acquire(stateDir, sessionID, operation)
}

func acquireSnapshotLockContext(ctx context.Context, stateDir, sessionID string, operation int) (func(), error) {
	return statelock.AcquireContext(ctx, stateDir, sessionID, operation)
}

// AcquireSessionReadLock keeps a retained snapshot and its audit record stable
// across a restore command, including the final RestoreEvents write.
func AcquireSessionReadLock(stateDir, sessionID string) (func(), error) {
	return statelock.Acquire(stateDir, sessionID, statelock.Shared)
}
