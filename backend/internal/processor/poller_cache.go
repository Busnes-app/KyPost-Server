package processor

// maxRetainedPollerResources bounds each cache of state handles and native
// mailbox clients. This bounds cache references, not live callers or process
// memory: existing runtime cleanups close handles after the last holder goes
// away. External IMAP sessions use their separate cache and explicit Close.
const maxRetainedPollerResources = 64

// retainPollerResource requires userMu. Do not Close an evicted resource:
// maintenance and polling can still hold it after releasing that mutex.
// ponytail: arbitrary eviction is enough for scans; no recency bookkeeping.
func retainPollerResource[T any](cache map[string]T, key string, value T) {
	if _, exists := cache[key]; !exists && len(cache) >= maxRetainedPollerResources {
		for old := range cache {
			delete(cache, old)
			break
		}
	}
	cache[key] = value
}
