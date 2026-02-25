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
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters"
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

// ValidateCallbackURL checks that a callback URL is safe to send requests to.
// It rejects private, link-local, and metadata IP ranges to prevent SSRF.
// Loopback addresses (127.0.0.0/8, ::1) are allowed for local development.
func ValidateCallbackURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("callback URL scheme must be https (got %q)", u.Scheme)
	}

	hostname := u.Hostname()

	// Resolve to IPs and check each one.
	ips, err := net.LookupHost(hostname)
	if err != nil {
		return fmt.Errorf("callback URL: cannot resolve host %q: %w", hostname, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if isSSRFTarget(ip) {
			return fmt.Errorf("callback URL resolves to private/reserved IP %s", ipStr)
		}
	}
	return nil
}

// ssrfRanges are CIDR blocks that must not be targeted by callbacks.
// Loopback (127.0.0.0/8, ::1) is intentionally excluded — it's needed for
// local development and the agent's own callback server.
var ssrfRanges = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // link-local / cloud metadata
		"fc00::/7",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

func isSSRFTarget(ip net.IP) bool {
	for _, n := range ssrfRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// DeliverResult POSTs a result payload to the given callback URL.
// signingKey is the raw agent bearer token; if non-empty an HMAC-SHA256 signature
// of the body is added as X-Clawvisor-Signature: sha256=<hex>.
// It is best-effort: errors are returned but never retried.
func DeliverResult(ctx context.Context, callbackURL string, payload *Payload, signingKey string) error {
	if callbackURL == "" {
		return nil
	}

	if err := ValidateCallbackURL(callbackURL); err != nil {
		return fmt.Errorf("callback: %w", err)
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
