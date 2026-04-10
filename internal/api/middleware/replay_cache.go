package middleware

import "time"

// ReplayCache detects replayed DeviceHMAC requests. The in-memory sync.Map
// implementation is the default; a Redis-backed implementation is used in
// multi-instance mode.
type ReplayCache interface {
	// Check returns true if the key has already been seen (replay detected).
	// Otherwise it stores the key and returns false.
	Check(key string) bool
}

// memoryReplayCache wraps the existing package-level sync.Map.
type memoryReplayCache struct{}

func NewMemoryReplayCache() ReplayCache {
	return memoryReplayCache{}
}

func (memoryReplayCache) Check(key string) bool {
	_, loaded := replayCache.LoadOrStore(key, time.Now())
	return loaded
}
