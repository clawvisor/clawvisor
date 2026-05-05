package proxy

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	defaultLeafCacheSize = 10_000
	leafCertValidity     = 397 * 24 * time.Hour
)

type LeafCertCache struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey

	capacity int
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List

	sf singleflight.Group
}

type leafEntry struct {
	host string
	cert *tls.Certificate
}

func NewLeafCertCache(ca *x509.Certificate, caKey *ecdsa.PrivateKey, capacity int) *LeafCertCache {
	if capacity <= 0 {
		capacity = defaultLeafCacheSize
	}
	return &LeafCertCache{
		ca:       ca,
		caKey:    caKey,
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
	}
}

func (c *LeafCertCache) Get(host string) (*tls.Certificate, error) {
	if cert := c.lookup(host); cert != nil {
		return cert, nil
	}
	v, err, _ := c.sf.Do(host, func() (any, error) {
		if cert := c.lookup(host); cert != nil {
			return cert, nil
		}
		cert, err := c.mint(host)
		if err != nil {
			return nil, err
		}
		c.store(host, cert)
		return cert, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*tls.Certificate), nil
}

func (c *LeafCertCache) lookup(host string) *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[host]
	if !ok {
		return nil
	}
	c.order.MoveToFront(el)
	return el.Value.(*leafEntry).cert
}

func (c *LeafCertCache) store(host string, cert *tls.Certificate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[host]; ok {
		c.order.MoveToFront(el)
		el.Value.(*leafEntry).cert = cert
		return
	}
	el := c.order.PushFront(&leafEntry{host: host, cert: cert})
	c.entries[host] = el
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*leafEntry).host)
	}
}

func (c *LeafCertCache) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(leafCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.ca, &key.PublicKey, c.caKey)
	if err != nil {
		return nil, fmt.Errorf("sign leaf cert for %s: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.ca.Raw},
		PrivateKey:  key,
		Leaf:        mustParse(der),
	}, nil
}

func mustParse(der []byte) *x509.Certificate {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic(fmt.Errorf("parse self-minted leaf cert: %w", err))
	}
	return cert
}
