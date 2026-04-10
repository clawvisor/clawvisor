package intent

// VerdictCacher is the interface for intent verification verdict caching.
// Both in-memory and Redis implementations satisfy this.
type VerdictCacher interface {
	Get(key cacheKey) (*VerificationVerdict, bool)
	Put(key cacheKey, verdict *VerificationVerdict)
	Cleanup()
}
