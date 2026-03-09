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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Payload is posted to the agent's callback URL.
type Payload struct {
	Type      string           `json:"type"`                 // "request" or "task"
	RequestID string           `json:"request_id,omitempty"` // populated when Type == "request"
	TaskID    string           `json:"task_id,omitempty"`    // populated when Type == "task"
	Status    string           `json:"status"`
	Result    *adapters.Result `json:"result,omitempty"`
	Error     string           `json:"error,omitempty"`
	AuditID   string           `json:"audit_id,omitempty"`
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DialContext: ssrfSafeDialContext,
	},
}

// ssrfSafeDialContext resolves DNS and checks the resulting IPs against the
// SSRF blocklist before connecting. This eliminates the TOCTOU gap that occurs
// when DNS is resolved separately in validation and again in http.Client.Do.
func ssrfSafeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("callback: invalid address %q: %w", address, err)
	}

	resolver := &net.Resolver{}
	ips, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("callback: cannot resolve host %q: %w", host, err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if isSSRFTarget(ip) {
			return nil, fmt.Errorf("callback: host %q resolves to blocked IP %s", host, ipStr)
		}
		// Dial the first safe IP directly, bypassing a second DNS resolution.
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ipStr, port))
	}
	return nil, fmt.Errorf("callback: no safe IPs found for host %q", host)
}

// requireHTTPS controls whether ValidateCallbackURL rejects http:// URLs
// for non-localhost hosts. Set via Init.
var requireHTTPS bool

// blockLoopback controls whether loopback IPs (127.0.0.0/8, ::1) are treated
// as SSRF targets. Enabled in production to prevent callbacks to the host itself.
var blockLoopback bool

// ValidateCallbackURL checks that a callback URL is parseable and uses an
// allowed scheme. IP-level SSRF checks are performed at connect time by
// ssrfSafeDialContext to prevent DNS rebinding.
func ValidateCallbackURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid callback URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("callback URL scheme must be http or https (got %q)", u.Scheme)
	}
	if requireHTTPS && u.Scheme == "http" {
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" {
			return fmt.Errorf("callback URL must use https (http only allowed for localhost)")
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

// allowedCIDRs are CIDR blocks exempt from SSRF blocking (configured at startup).
var allowedCIDRs []*net.IPNet

// Init parses the given CIDR strings into an allowlist that exempts matching IPs
// from the SSRF check, sets the HTTPS enforcement flag, and optionally blocks
// loopback addresses (for production deployments). Call once at startup.
func Init(cidrs []string, httpsRequired, blockLoopbackAddrs bool) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		nets = append(nets, n)
	}
	allowedCIDRs = nets
	requireHTTPS = httpsRequired
	blockLoopback = blockLoopbackAddrs
}

func isSSRFTarget(ip net.IP) bool {
	for _, n := range allowedCIDRs {
		if n.Contains(ip) {
			return false
		}
	}
	if blockLoopback && ip.IsLoopback() {
		return true
	}
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
		idKey, idVal := payloadIDAttr(payload)
		slog.Warn("callback: delivery failed",
			"url", callbackURL,
			"type", payload.Type,
			idKey, idVal,
			"err", err,
		)
		return fmt.Errorf("callback: POST %s: %w", callbackURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		idKey, idVal := payloadIDAttr(payload)
		slog.Warn("callback: non-success response",
			"url", callbackURL,
			"type", payload.Type,
			idKey, idVal,
			"status_code", resp.StatusCode,
			"response_body", string(respBody),
		)
		return fmt.Errorf("callback: POST %s returned %d", callbackURL, resp.StatusCode)
	}

	return nil
}

// payloadIDAttr returns the log key/value for the payload's ID field.
func payloadIDAttr(p *Payload) (string, string) {
	if p.TaskID != "" {
		return "task_id", p.TaskID
	}
	return "request_id", p.RequestID
}
