// Command cvis-e2e is a standalone E2E encryption helper for environments
// without Node.js. Same CLI as e2e.mjs.
//
// Usage: cvis-e2e --url <daemon_url> --token <agent_token> --body '<json>'
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	url := flag.String("url", "", "Daemon URL")
	token := flag.String("token", "", "Agent token")
	body := flag.String("body", "", "JSON body to send")
	flag.Parse()

	if *url == "" || *token == "" || *body == "" {
		fmt.Fprintln(os.Stderr, "Usage: cvis-e2e --url <daemon_url> --token <agent_token> --body '<json>'")
		os.Exit(1)
	}

	result, err := gatewayRequest(*url, *token, *body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var pretty bytes.Buffer
	json.Indent(&pretty, result, "", "  ")
	fmt.Println(pretty.String())
}

func gatewayRequest(baseURL, agentToken, bodyJSON string) ([]byte, error) {
	// Fetch daemon's public key.
	daemonKey, err := fetchDaemonKey(baseURL)
	if err != nil {
		return nil, fmt.Errorf("fetching daemon key: %w", err)
	}

	daemonPubBytes, err := base64.StdEncoding.DecodeString(daemonKey.X25519)
	if err != nil {
		return nil, fmt.Errorf("decoding daemon public key: %w", err)
	}

	daemonPub, err := ecdh.X25519().NewPublicKey(daemonPubBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing daemon public key: %w", err)
	}

	// Generate ephemeral keypair.
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral key: %w", err)
	}

	// ECDH.
	shared, err := ephPriv.ECDH(daemonPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}

	// Encrypt request body.
	ciphertext, err := encrypt(shared, []byte(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("encrypting: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(ciphertext)

	req, err := http.NewRequest("POST", baseURL+"/api/gateway/request", bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Clawvisor-E2E", "aes-256-gcm")
	req.Header.Set("X-Clawvisor-Ephemeral-Key", base64.StdEncoding.EncodeToString(ephPriv.PublicKey().Bytes()))

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.Header.Get("X-Clawvisor-E2E") != "" {
		decrypted, err := decryptResponse(shared, string(respBody))
		if err != nil {
			return nil, fmt.Errorf("decrypting response: %w", err)
		}
		return decrypted, nil
	}

	return respBody, nil
}

type daemonKeys struct {
	DaemonID string `json:"daemon_id"`
	X25519   string `json:"x25519"`
}

func fetchDaemonKey(baseURL string) (*daemonKeys, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/.well-known/clawvisor-keys")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key endpoint returned %d", resp.StatusCode)
	}

	var keys daemonKeys
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, err
	}
	return &keys, nil
}

func encrypt(shared, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(shared)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Seal appends ciphertext+tag after nonce.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptResponse(shared []byte, ciphertextB64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, err
	}
	if len(data) < 12+16 {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce := data[:12]
	encData := data[12:]

	block, err := aes.NewCipher(shared)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, encData, nil)
}
