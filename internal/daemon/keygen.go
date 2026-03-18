package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
	"gopkg.in/yaml.v3"
)

// ensureRelayKeys generates Ed25519 and X25519 keypairs if they don't exist.
func ensureRelayKeys(dataDir string) error {
	ed25519KeyPath := filepath.Join(dataDir, "daemon-ed25519.key")
	x25519KeyPath := filepath.Join(dataDir, "daemon-x25519.key")

	// Generate Ed25519 key for relay authentication.
	if _, err := os.Stat(ed25519KeyPath); os.IsNotExist(err) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("generating Ed25519 key: %w", err)
		}
		block := &pem.Block{
			Type:  "ED25519 PRIVATE KEY",
			Bytes: priv.Seed(),
		}
		if err := os.WriteFile(ed25519KeyPath, pem.EncodeToMemory(block), 0600); err != nil {
			return fmt.Errorf("writing Ed25519 key: %w", err)
		}
	}

	// Generate X25519 key for E2E encryption.
	if _, err := os.Stat(x25519KeyPath); os.IsNotExist(err) {
		privKey := make([]byte, curve25519.ScalarSize)
		if _, err := rand.Read(privKey); err != nil {
			return fmt.Errorf("generating X25519 key: %w", err)
		}
		if err := os.WriteFile(x25519KeyPath, privKey, 0600); err != nil {
			return fmt.Errorf("writing X25519 key: %w", err)
		}
	}

	return nil
}

// registerWithRelay registers the daemon's Ed25519 public key with the relay
// service and writes the returned daemon_id to config.yaml.
func registerWithRelay(dataDir, relayURL string) error {
	configPath := filepath.Join(dataDir, "config.yaml")

	// Check if daemon_id is already in config.
	if id, _ := readDaemonID(configPath); id != "" {
		return nil
	}

	// Load Ed25519 private key to derive public key.
	ed25519KeyPath := filepath.Join(dataDir, "daemon-ed25519.key")
	priv, err := loadEd25519Key(ed25519KeyPath)
	if err != nil {
		return fmt.Errorf("loading Ed25519 key: %w", err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	body, _ := json.Marshal(map[string]string{
		"public_key": base64.StdEncoding.EncodeToString(pub),
		"version":    "1.0.0",
	})

	httpURL := strings.Replace(relayURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(httpURL+"/api/relay/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("registering with relay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay registration failed: status %d", resp.StatusCode)
	}

	var result struct {
		DaemonID string `json:"daemon_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parsing relay response: %w", err)
	}

	return patchDaemonID(configPath, result.DaemonID)
}

// loadEd25519Key reads a PEM-encoded Ed25519 private key seed.
func loadEd25519Key(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	return ed25519.NewKeyFromSeed(block.Bytes), nil
}

// loadX25519Key reads a raw 32-byte X25519 private key.
func loadX25519Key(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != curve25519.ScalarSize {
		return nil, fmt.Errorf("X25519 key must be %d bytes, got %d", curve25519.ScalarSize, len(data))
	}
	return data, nil
}

// readDaemonID reads relay.daemon_id from config.yaml.
func readDaemonID(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", err
	}
	relayCfg, ok := raw["relay"].(map[string]interface{})
	if !ok {
		return "", nil
	}
	id, _ := relayCfg["daemon_id"].(string)
	return id, nil
}

// patchDaemonID writes relay.daemon_id into config.yaml.
func patchDaemonID(configPath, daemonID string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	relayCfg, ok := raw["relay"].(map[string]interface{})
	if !ok {
		relayCfg = map[string]interface{}{}
		raw["relay"] = relayCfg
	}
	relayCfg["daemon_id"] = daemonID
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0600)
}
