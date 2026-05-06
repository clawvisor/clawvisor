package proxy

// Phase 0.2 — edge TLS cert (the cert the listener serves on
// proxy.clawvisor.com) is decoupled from the MITM CA path. The cloud
// binary wires this provider so the listener's GetCertificate returns
// a real publicly-trusted leaf, not a forged-from-internal-CA leaf.
//
// Reload model:
//   - Cert+key files live at fixed paths (typically a Kubernetes Secret
//     mounted into the pod under /etc/edge-tls/).
//   - A periodic re-stat loop watches mtime+size; on change, reloads the
//     cert pair and atomically swaps an atomic.Pointer[tls.Certificate].
//   - Mid-handshake reloads are safe: GetCertificate captures the
//     pointer once per handshake; new handshakes pick up the new cert,
//     in-flight ones keep the old.
//
// fsnotify-based instant reload is the production target (cuts reload
// latency from "<5 min" to "subsecond"). It needs the fsnotify
// dependency, which is added at the same time the cloud binary lands;
// for now the periodic-restat path is the fallback the plan documents.

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// EdgeCertProvider returns the current edge TLS cert. The pointer
// returned is safe for concurrent use; callers should NOT mutate it.
type EdgeCertProvider interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Close stops any background reload loop. Safe to call multiple times.
	Close() error
}

// FileEdgeCertProvider serves the edge cert from a cert + key file pair
// on disk and reloads them on change.
type FileEdgeCertProvider struct {
	certPath string
	keyPath  string
	current  atomic.Pointer[tls.Certificate]
	stop     chan struct{}
	closed   atomic.Bool

	// Stat baseline for the periodic reload loop. Written once during
	// construction before the goroutine starts, then owned exclusively
	// by run().
	lastCertMod  time.Time
	lastCertSize int64
	lastKeyMod   time.Time
	lastKeySize  int64
}

// NewFileEdgeCertProvider loads the initial cert+key pair and starts a
// periodic re-stat loop (default 60s) for reload. The reload interval is
// configurable for tests.
func NewFileEdgeCertProvider(certPath, keyPath string, reloadEvery time.Duration) (*FileEdgeCertProvider, error) {
	if certPath == "" || keyPath == "" {
		return nil, errors.New("edge cert path and key path are required")
	}
	if reloadEvery <= 0 {
		reloadEvery = 60 * time.Second
	}
	p := &FileEdgeCertProvider{
		certPath: certPath,
		keyPath:  keyPath,
		stop:     make(chan struct{}),
	}
	if err := p.reload(); err != nil {
		return nil, fmt.Errorf("initial edge cert load: %w", err)
	}
	// Capture the baseline stat synchronously so a slow goroutine start
	// can't race with a writer and adopt the post-write mtime/size as
	// "no change."
	if certInfo, err := os.Stat(p.certPath); err == nil {
		p.lastCertMod = certInfo.ModTime()
		p.lastCertSize = certInfo.Size()
	}
	if keyInfo, err := os.Stat(p.keyPath); err == nil {
		p.lastKeyMod = keyInfo.ModTime()
		p.lastKeySize = keyInfo.Size()
	}
	go p.run(reloadEvery)
	return p, nil
}

// GetCertificate satisfies tls.Config.GetCertificate. ClientHelloInfo is
// ignored because we serve a single SNI/host on the listener.
func (p *FileEdgeCertProvider) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := p.current.Load()
	if cert == nil {
		return nil, errors.New("edge cert not loaded")
	}
	return cert, nil
}

// Close stops the background reload loop.
func (p *FileEdgeCertProvider) Close() error {
	if p.closed.CompareAndSwap(false, true) {
		close(p.stop)
	}
	return nil
}

func (p *FileEdgeCertProvider) run(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			certInfo, err := os.Stat(p.certPath)
			if err != nil {
				continue
			}
			keyInfo, err := os.Stat(p.keyPath)
			if err != nil {
				continue
			}
			changed := !certInfo.ModTime().Equal(p.lastCertMod) ||
				!keyInfo.ModTime().Equal(p.lastKeyMod) ||
				certInfo.Size() != p.lastCertSize ||
				keyInfo.Size() != p.lastKeySize
			if !changed {
				continue
			}
			if err := p.reload(); err != nil {
				// Don't update lastMod — we want to retry next tick.
				continue
			}
			p.lastCertMod = certInfo.ModTime()
			p.lastCertSize = certInfo.Size()
			p.lastKeyMod = keyInfo.ModTime()
			p.lastKeySize = keyInfo.Size()
		}
	}
}

func (p *FileEdgeCertProvider) reload() error {
	cert, err := tls.LoadX509KeyPair(p.certPath, p.keyPath)
	if err != nil {
		return err
	}
	p.current.Store(&cert)
	return nil
}

// reloadOnce is exposed for tests so they don't need to wait for the
// periodic loop.
func (p *FileEdgeCertProvider) reloadOnce() error {
	return p.reload()
}
