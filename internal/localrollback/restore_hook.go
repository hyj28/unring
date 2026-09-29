package localrollback

import "sync"

var restoreMetadataHookState struct {
	sync.RWMutex
	hook func(string)
}

// SetRestoreMetadataHookForTest observes directory finalization and returns a
// restore function. Production leaves the hook unset.
func SetRestoreMetadataHookForTest(hook func(string)) func() {
	restoreMetadataHookState.Lock()
	previous := restoreMetadataHookState.hook
	restoreMetadataHookState.hook = hook
	restoreMetadataHookState.Unlock()
	return func() {
		restoreMetadataHookState.Lock()
		restoreMetadataHookState.hook = previous
		restoreMetadataHookState.Unlock()
	}
}

func observeRestoreMetadata(path string) {
	restoreMetadataHookState.RLock()
	hook := restoreMetadataHookState.hook
	restoreMetadataHookState.RUnlock()
	if hook != nil {
		hook(path)
	}
}
