package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"
)

// stubCASource is a test CASource backed by an in-memory map keyed by
// userID. The latest version stored wins on Get.
type stubCASource struct {
	mu    sync.Mutex
	users map[string]stubCA
}

type stubCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	version int
}

func (s *stubCASource) GetUserCA(userID string) (*x509.Certificate, *ecdsa.PrivateKey, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ca, ok := s.users[userID]
	if !ok {
		return nil, nil, 0, errors.New("no CA for user")
	}
	return ca.cert, ca.key, ca.version, nil
}

func (s *stubCASource) set(userID string, version int) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ca-" + userID},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	cert, _ := x509.ParseCertificate(der)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[userID] = stubCA{cert: cert, key: priv, version: version}
}

func TestMultiUserLeafCertCache_PerUserIsolation(t *testing.T) {
	src := &stubCASource{users: map[string]stubCA{}}
	src.set("alice", 1)
	src.set("bob", 1)

	cache := NewMultiUserLeafCertCache(src, 16)

	aliceCert, err := cache.Get("alice", "api.example.com")
	if err != nil {
		t.Fatalf("Get(alice): %v", err)
	}
	bobCert, err := cache.Get("bob", "api.example.com")
	if err != nil {
		t.Fatalf("Get(bob): %v", err)
	}

	// The leaf certs MUST chain to different CAs (since Alice and Bob
	// have distinct CAs in the source).
	if string(aliceCert.Certificate[1]) == string(bobCert.Certificate[1]) {
		t.Errorf("Alice's leaf chains to the same CA as Bob's; per-user isolation failed")
	}
}

func TestMultiUserLeafCertCache_CARotationEvicts(t *testing.T) {
	src := &stubCASource{users: map[string]stubCA{}}
	src.set("alice", 1)

	cache := NewMultiUserLeafCertCache(src, 16)

	cert1, err := cache.Get("alice", "api.example.com")
	if err != nil {
		t.Fatalf("Get(v1): %v", err)
	}

	// Bump to version 2 with a new CA.
	src.set("alice", 2)

	cert2, err := cache.Get("alice", "api.example.com")
	if err != nil {
		t.Fatalf("Get(v2): %v", err)
	}

	// Different shard ⇒ different leaf bytes.
	if len(cert1.Certificate) > 0 && len(cert2.Certificate) > 0 &&
		string(cert1.Certificate[0]) == string(cert2.Certificate[0]) {
		t.Errorf("rotation should produce a new leaf; got identical bytes")
	}

	shards, _ := cache.Stats()
	if shards < 2 {
		t.Errorf("expected at least 2 shards after rotation, got %d", shards)
	}

	cache.EvictUserCAVersion("alice", 1)
	shards, _ = cache.Stats()
	if shards != 1 {
		t.Errorf("after EvictUserCAVersion(alice,1) expected 1 shard; got %d", shards)
	}
}

func TestMultiUserLeafCertCache_EvictUser(t *testing.T) {
	src := &stubCASource{users: map[string]stubCA{}}
	src.set("alice", 1)
	src.set("alice", 2) // overwrites alice; both old and new versions accessible via direct Get
	src.set("bob", 1)

	cache := NewMultiUserLeafCertCache(src, 16)
	if _, err := cache.Get("alice", "h1"); err != nil {
		t.Fatalf("Get(alice): %v", err)
	}
	if _, err := cache.Get("bob", "h1"); err != nil {
		t.Fatalf("Get(bob): %v", err)
	}
	cache.EvictUser("alice")
	shards, _ := cache.Stats()
	if shards != 1 {
		t.Errorf("expected 1 shard after EvictUser(alice); got %d", shards)
	}
}

func TestMultiUserLeafCertCache_RejectsEmptyUserID(t *testing.T) {
	src := &stubCASource{users: map[string]stubCA{}}
	cache := NewMultiUserLeafCertCache(src, 16)
	if _, err := cache.Get("", "h1"); err == nil {
		t.Error("expected error for empty userID")
	}
}

func TestMultiUserLeafCertCache_NilSourceReturnsError(t *testing.T) {
	cache := NewMultiUserLeafCertCache(nil, 16)
	if _, err := cache.Get("u", "h"); err == nil {
		t.Error("expected error when CASource is nil")
	}
}

func TestMultiUserLeafCertCache_NilReceiver(t *testing.T) {
	var cache *MultiUserLeafCertCache
	if _, err := cache.Get("u", "h"); err == nil {
		t.Error("expected error from nil receiver Get")
	}
	cache.EvictUser("u")     // should not panic
	cache.EvictUserCAVersion("u", 1)
	if shards, leafs := cache.Stats(); shards != 0 || leafs != 0 {
		t.Errorf("nil receiver Stats = (%d, %d), want (0, 0)", shards, leafs)
	}
}
