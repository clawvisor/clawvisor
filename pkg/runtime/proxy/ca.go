package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFilename = "ca.pem"
	caKeyFilename  = "ca-key.pem"
)

func GenerateCA(dataDir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Clawvisor Runtime Proxy"},
			CommonName:   "Clawvisor Runtime Proxy Local CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}

	if err := writePEM(filepath.Join(dataDir, caCertFilename), "CERTIFICATE", certDER, 0o644); err != nil {
		return nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	if err := writePEM(filepath.Join(dataDir, caKeyFilename), "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

func LoadCA(dataDir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readCertPEM(filepath.Join(dataDir, caCertFilename))
	if err != nil {
		return nil, nil, err
	}
	key, err := readECKeyPEM(filepath.Join(dataDir, caKeyFilename))
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func LoadOrGenerateCA(dataDir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, key, err := LoadCA(dataDir)
	if err == nil {
		return cert, key, nil
	}
	certPath := filepath.Join(dataDir, caCertFilename)
	if _, statErr := os.Stat(certPath); os.IsNotExist(statErr) {
		return GenerateCA(dataDir)
	}
	return nil, nil, fmt.Errorf("CA exists but cannot be loaded: %w", err)
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}

func readCertPEM(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate %s: %w", path, err)
	}
	return cert, nil
}

func readECKeyPEM(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse EC key %s: %w", path, err)
	}
	return key, nil
}
