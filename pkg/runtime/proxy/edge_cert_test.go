package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateLeafKeyPair creates a self-signed ECDSA P-256 cert and key,
// writes both to the given paths in PEM form, and returns the certificate
// bytes for assertion comparisons.
func generateLeafKeyPair(t *testing.T, certPath, keyPath, commonName string) []byte {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{commonName},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certDER
}

func TestFileEdgeCertProvider_LoadAndServe(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	wantDER := generateLeafKeyPair(t, certPath, keyPath, "edge.example.com")

	p, err := NewFileEdgeCertProvider(certPath, keyPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewFileEdgeCertProvider: %v", err)
	}
	defer p.Close()

	cert, err := p.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("cert.Certificate is empty")
	}
	if string(cert.Certificate[0]) != string(wantDER) {
		t.Errorf("served cert does not match initial bytes")
	}
}

func TestFileEdgeCertProvider_ReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	_ = generateLeafKeyPair(t, certPath, keyPath, "v1.example.com")

	p, err := NewFileEdgeCertProvider(certPath, keyPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewFileEdgeCertProvider: %v", err)
	}
	defer p.Close()

	first, err := p.GetCertificate(nil)
	if err != nil {
		t.Fatalf("first GetCertificate: %v", err)
	}

	// Replace the cert+key with new content. Use a different DNS name so
	// the leaf bytes definitely change.
	newDER := generateLeafKeyPair(t, certPath, keyPath, "v2.example.com")
	// Bump mtime explicitly to make sure the periodic loop sees it.
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(certPath, now, now)
	_ = os.Chtimes(keyPath, now, now)

	// Wait up to 2s for reload, polling.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := p.GetCertificate(nil)
		if err == nil && len(current.Certificate) > 0 && string(current.Certificate[0]) == string(newDER) {
			return // reloaded successfully
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("provider did not reload to new cert within 2s; first cert leaf %x...", first.Certificate[0][:8])
}

func TestFileEdgeCertProvider_ReloadFailureKeepsOldCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	wantDER := generateLeafKeyPair(t, certPath, keyPath, "stable.example.com")

	p, err := NewFileEdgeCertProvider(certPath, keyPath, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewFileEdgeCertProvider: %v", err)
	}
	defer p.Close()

	// Corrupt the cert file. The reload must FAIL and the previous cert
	// must stay served.
	if err := os.WriteFile(certPath, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	now := time.Now().Add(time.Second)
	_ = os.Chtimes(certPath, now, now)

	// Force a reload attempt directly (simulates the periodic loop).
	if err := p.reloadOnce(); err == nil {
		t.Fatal("expected reload error on corrupted cert, got nil")
	}

	cert, err := p.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(cert.Certificate) == 0 || string(cert.Certificate[0]) != string(wantDER) {
		t.Errorf("expected provider to keep serving original cert after failed reload")
	}
}

func TestFileEdgeCertProvider_RequiresPaths(t *testing.T) {
	if _, err := NewFileEdgeCertProvider("", "/key", time.Second); err == nil {
		t.Error("expected error when certPath empty")
	}
	if _, err := NewFileEdgeCertProvider("/cert", "", time.Second); err == nil {
		t.Error("expected error when keyPath empty")
	}
}
