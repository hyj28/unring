package localrollback

import (
	"context"
	"sync"
)

var scanPathHookState struct {
	sync.RWMutex
	hook func(context.Context, string)
}

// SetScanPathHookForTest observes filesystem-walk paths and returns a restore
// function. Tests use it to pause a real walk at a deterministic cancellation
// point; production leaves the hook unset.
func SetScanPathHookForTest(hook func(context.Context, string)) func() {
	scanPathHookState.Lock()
	previous := scanPathHookState.hook
	scanPathHookState.hook = hook
	scanPathHookState.Unlock()
	return func() {
		scanPathHookState.Lock()
		scanPathHookState.hook = previous
		scanPathHookState.Unlock()
	}
}

func observeScanPath(ctx context.Context, path string) {
	scanPathHookState.RLock()
	hook := scanPathHookState.hook
	scanPathHookState.RUnlock()
	if hook != nil {
		hook(ctx, path)
	}
}
