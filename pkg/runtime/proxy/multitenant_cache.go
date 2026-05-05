package proxy

// Phase 0.2 — multi-tenant leaf cert cache.
//
// The single-CA LeafCertCache works for daemon mode (one user, one CA per
// proxy install). Cloud needs per-user CAs at MITM time, with leaf certs
// keyed by (userID, caVersion, host) so:
//   1. CA rotation/revocation evicts by (userID, caVersion) prefix —
//      old leafs never get re-served under a new CA;
//   2. The singleflight de-dup for leaf minting must be keyed identically
//      so two users hitting the same host don't accidentally share work
//      done with the wrong CA.
//
// This cache wraps per-(userID,caVersion) LeafCertCache instances. The
// lookup path takes the user's CA via a CASource so the cache itself
// doesn't have to know about CA storage / rotation policy — that's the
// caller's concern.

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
)

// CASource resolves the active CA for a user at MITM time. The cloud
// implementation will look up the user's CA from a wrapped-DEK store
// (per the per-user PKI plan). caVersion is monotonically increasing
// per user; rotation bumps the version.
type CASource interface {
	GetUserCA(userID string) (cert *x509.Certificate, key *ecdsa.PrivateKey, caVersion int, err error)
}

// MultiUserLeafCertCache holds per-user LeafCertCache shards keyed by
// (userID, caVersion). Each shard is a normal LeafCertCache. Eviction
// of a (userID, caVersion) shard removes all leafs minted under it.
type MultiUserLeafCertCache struct {
	source       CASource
	leafCapacity int

	mu     sync.RWMutex
	shards map[shardKey]*LeafCertCache
}

type shardKey struct {
	userID    string
	caVersion int
}

// NewMultiUserLeafCertCache wires a CASource and a per-shard leaf
// capacity. leafCapacity defaults to 256 if non-positive.
func NewMultiUserLeafCertCache(source CASource, leafCapacity int) *MultiUserLeafCertCache {
	if leafCapacity <= 0 {
		leafCapacity = 256
	}
	return &MultiUserLeafCertCache{
		source:       source,
		leafCapacity: leafCapacity,
		shards:       make(map[shardKey]*LeafCertCache),
	}
}

// Get returns a leaf cert for the (userID, host) pair, minting it via
// the user's current CA if needed. If the user's caVersion has changed
// since the last call, a new shard is allocated; old shards remain
// reachable via EvictUserCAVersion (cloud calls this on rotation).
func (m *MultiUserLeafCertCache) Get(userID, host string) (*tls.Certificate, error) {
	if m == nil {
		return nil, errors.New("MultiUserLeafCertCache is nil")
	}
	if userID == "" {
		return nil, errors.New("userID is required")
	}
	if m.source == nil {
		return nil, errors.New("CASource is required")
	}
	cert, key, version, err := m.source.GetUserCA(userID)
	if err != nil {
		return nil, err
	}
	shard := m.getOrCreateShard(userID, version, cert, key)
	return shard.Get(host)
}

// EvictUserCAVersion drops all leafs minted under (userID, caVersion).
// Call this from cloud's CA rotation path. Idempotent.
func (m *MultiUserLeafCertCache) EvictUserCAVersion(userID string, caVersion int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.shards, shardKey{userID: userID, caVersion: caVersion})
}

// EvictUser drops every shard for the user (all caVersions). Use on
// user-level kill-switch.
func (m *MultiUserLeafCertCache) EvictUser(userID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.shards {
		if k.userID == userID {
			delete(m.shards, k)
		}
	}
}

// Stats returns the number of active shards and the sum of leafs across
// all shards. Cheap to call; intended for metrics export.
func (m *MultiUserLeafCertCache) Stats() (shards int, totalLeafs int) {
	if m == nil {
		return 0, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.shards {
		c.mu.Lock()
		totalLeafs += c.order.Len()
		c.mu.Unlock()
	}
	return len(m.shards), totalLeafs
}

func (m *MultiUserLeafCertCache) getOrCreateShard(userID string, version int, cert *x509.Certificate, key *ecdsa.PrivateKey) *LeafCertCache {
	k := shardKey{userID: userID, caVersion: version}
	m.mu.RLock()
	if existing, ok := m.shards[k]; ok {
		m.mu.RUnlock()
		return existing
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.shards[k]; ok {
		return existing
	}
	shard := NewLeafCertCache(cert, key, m.leafCapacity)
	m.shards[k] = shard
	return shard
}
