package handlers

// DedupCache is the interface for request content deduplication.
// Both in-memory and Redis implementations satisfy this.
type DedupCache interface {
	Get(key dedupKey) (any, bool)
	Put(key dedupKey, value any)
	Stop()
}
