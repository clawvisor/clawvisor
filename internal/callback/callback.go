// Package callback delivers gateway results to agent callback URLs (OpenClaw inbound endpoints).
package callback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ericlevine/clawvisor/internal/adapters"
)

// Payload is posted to the agent's callback URL.
type Payload struct {
	RequestID string           `json:"request_id"`
	Status    string           `json:"status"` // "executed" | "denied" | "timeout" | "error"
	Result    *adapters.Result `json:"result,omitempty"`
	Error     string           `json:"error,omitempty"` // populated when status == "error"
	AuditID   string           `json:"audit_id"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// DeliverResult POSTs a result payload to the given callback URL.
// signingKey is the raw agent bearer token; if non-empty an HMAC-SHA256 signature
// of the body is added as X-Clawvisor-Signature: sha256=<hex>.
// It is best-effort: errors are returned but never retried.
func DeliverResult(ctx context.Context, callbackURL string, payload *Payload, signingKey string) error {
	if callbackURL == "" {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("callback: marshaling payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("callback: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if signingKey != "" {
		mac := hmac.New(sha256.New, []byte(signingKey))
		mac.Write(body)
		req.Header.Set("X-Clawvisor-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callback: POST %s: %w", callbackURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("callback: POST %s returned %d", callbackURL, resp.StatusCode)
	}
	return nil
}
