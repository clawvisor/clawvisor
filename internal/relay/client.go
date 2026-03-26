package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/clawvisor/pkg/config"
)

const (
	// writeTimeout bounds how long a single WebSocket write can block.
	writeTimeout = 10 * time.Second

	// authTimeout bounds the entire challenge-response handshake.
	authTimeout = 15 * time.Second

	// readTimeout detects silent relay disappearance faster than TCP keepalive.
	readTimeout = 90 * time.Second

	// pingInterval keeps the connection alive during idle periods.
	pingInterval = 30 * time.Second

	// maxConcurrentRequests caps in-flight request handler goroutines.
	maxConcurrentRequests = 50

	// maxReadSize limits the size of a single incoming WebSocket message (1 MB).
	maxReadSize = 1 << 20

	// maxConsecutiveAuthFailures triggers error-level logging when exceeded.
	maxConsecutiveAuthFailures = 3
)

// Client manages a persistent WebSocket connection to the cloud relay.
type Client struct {
	relayURL   string
	daemonID   string
	privateKey ed25519.PrivateKey
	handler    http.Handler
	logger     *slog.Logger

	baseDelay    time.Duration
	maxDelay     time.Duration
	authTimeout  time.Duration // overridable for tests; defaults to authTimeout const

	mu         sync.Mutex
	conn       *websocket.Conn
	connected  bool
	connClose  context.CancelFunc // cancels the per-connection context to tear down on write error
	registered bool               // true after successful registration with this relay
}

// New creates a relay client. The handler can be nil at construction time
// and set later via SetHandler before calling Run.
func New(cfg config.RelayConfig, key ed25519.PrivateKey,
	handler http.Handler, logger *slog.Logger) *Client {

	baseDelay, _ := time.ParseDuration(cfg.ReconnectBaseDelay)
	if baseDelay <= 0 {
		baseDelay = time.Second
	}
	maxDelay, _ := time.ParseDuration(cfg.ReconnectMaxDelay)
	if maxDelay <= 0 {
		maxDelay = 60 * time.Second
	}

	return &Client{
		relayURL:    cfg.URL,
		daemonID:    cfg.DaemonID,
		privateKey:  key,
		handler:     handler,
		logger:      logger,
		baseDelay:   baseDelay,
		maxDelay:    maxDelay,
		authTimeout: authTimeout,
	}
}

// SetHandler replaces the HTTP handler used to dispatch relay requests.
// Must be called before Run.
func (c *Client) SetHandler(h http.Handler) {
	c.handler = h
}

// register posts the daemon's Ed25519 public key to the relay's registration
// endpoint. If the relay already knows this key it returns the existing
// daemon_id; otherwise it creates a new one. The client updates its daemonID
// in-memory so subsequent connections use the correct identity.
func (c *Client) register(ctx context.Context) error {
	pub := c.privateKey.Public().(ed25519.PublicKey)
	body, _ := json.Marshal(map[string]string{
		"public_key": base64.StdEncoding.EncodeToString(pub),
		"version":    "1.0.0",
	})

	httpURL := strings.Replace(c.relayURL, "wss://", "https://", 1)
	httpURL = strings.Replace(httpURL, "ws://", "http://", 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL+"/api/relay/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
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

	c.daemonID = result.DaemonID
	c.registered = true
	c.logger.Info("relay: registered with relay", "daemon_id", result.DaemonID)
	return nil
}

// Run connects to the relay and blocks until ctx is cancelled.
// Handles reconnection with exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	// Register with the relay on first startup to ensure this daemon's key
	// is known. This is idempotent — re-registering returns the existing
	// daemon_id if the key is already known.
	if !c.registered {
		if err := c.register(ctx); err != nil {
			c.logger.Warn("relay: initial registration failed, will retry on auth failure", "err", err)
		}
	}

	delay := c.baseDelay
	consecutiveAuthFailures := 0

	for {
		connectedAt, err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// On auth failure, try re-registering to get a valid daemon_id
		// for this relay (e.g. after switching from production to staging).
		if err != nil && isAuthError(err) {
			consecutiveAuthFailures++
			if regErr := c.register(ctx); regErr != nil {
				c.logger.Warn("relay: re-registration failed", "err", regErr)
			}
			if consecutiveAuthFailures >= maxConsecutiveAuthFailures {
				c.logger.Error("relay auth failing repeatedly — check daemon key/config",
					"err", err, "consecutive_failures", consecutiveAuthFailures)
			}
		} else {
			consecutiveAuthFailures = 0
		}

		// Reset backoff if the connection was healthy for a meaningful period
		// (stayed up longer than the current delay). This avoids permanently
		// ratcheting to the max delay after a single transient failure.
		if connectedAt.After(time.Time{}) && time.Since(connectedAt) > delay {
			delay = c.baseDelay
		}

		c.logger.Warn("relay connection lost, reconnecting",
			"err", err, "delay", delay)

		// Apply ±25% jitter.
		jittered := applyJitter(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jittered):
		}

		// Exponential backoff: double the delay, cap at max.
		delay *= 2
		if delay > c.maxDelay {
			delay = c.maxDelay
		}
	}
}

// Connected returns true if the WebSocket connection is active.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// DaemonURL returns the public URL for this daemon.
func (c *Client) DaemonURL() string {
	// Derive the public URL from the configured relay URL.
	base := c.relayURL
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	return fmt.Sprintf("%s/d/%s", base, c.daemonID)
}

// connectAndServe establishes a WebSocket connection, authenticates, and
// processes frames until the connection drops or ctx is cancelled. Returns
// the time the connection became established (for backoff reset decisions).
func (c *Client) connectAndServe(ctx context.Context) (connectedAt time.Time, err error) {
	headers := http.Header{}
	headers.Set("X-Daemon-ID", c.daemonID)

	conn, _, dialErr := websocket.Dial(ctx, c.relayURL+"/ws", &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if dialErr != nil {
		return time.Time{}, fmt.Errorf("dialing relay: %w", dialErr)
	}

	// Limit incoming message size to prevent OOM from oversized frames.
	conn.SetReadLimit(maxReadSize)

	// Create a per-connection context so write errors can tear down the
	// read loop immediately instead of waiting for the read timeout.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// Authenticate via challenge-response with a bounded timeout.
	authCtx, authCancel := context.WithTimeout(connCtx, c.authTimeout)
	authErr := c.authenticate(authCtx, conn)
	authCancel()
	if authErr != nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return time.Time{}, fmt.Errorf("relay auth: %w", authErr)
	}

	connectedAt = time.Now()

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.connClose = connCancel
	c.mu.Unlock()

	// Use a WaitGroup to drain in-flight handlers before closing the conn.
	var wg sync.WaitGroup
	// Semaphore to bound concurrent request handlers.
	sem := make(chan struct{}, maxConcurrentRequests)

	defer func() {
		// Mark disconnected first so new sendFrame calls bail out.
		c.mu.Lock()
		c.conn = nil
		c.connected = false
		c.connClose = nil
		c.mu.Unlock()

		// Wait for in-flight request handlers to finish before closing
		// the connection, so their response writes can complete.
		wg.Wait()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.logger.Info("relay connected", "daemon_id", c.daemonID)

	// Send periodic pings to keep the connection alive. The relay may not
	// send traffic during idle periods, so without client-side pings the
	// read timeout fires and we needlessly reconnect.
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				if err := c.sendFrame(FramePing, "", nil); err != nil {
					c.logger.Warn("relay: ping write failed, closing connection", "err", err)
					connCancel() // tear down read loop
					return
				}
			}
		}
	}()

	// Read loop.
	for {
		readCtx, readCancel := context.WithTimeout(connCtx, readTimeout)
		_, data, readErr := conn.Read(readCtx)
		readCancel()
		if readErr != nil {
			return connectedAt, fmt.Errorf("reading frame: %w", readErr)
		}

		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			c.logger.Warn("relay: invalid frame", "err", err)
			continue
		}

		switch frame.Type {
		case FrameHTTPRequest:
			var payload HTTPRequestPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				c.logger.Warn("relay: invalid request payload", "err", err)
				continue
			}
			// Acquire semaphore slot (bounded concurrency).
			select {
			case sem <- struct{}{}:
			default:
				c.logger.Warn("relay: too many concurrent requests, dropping", "id", frame.ID)
				go c.sendResponse(frame.ID, HTTPResponsePayload{
					Status:  http.StatusServiceUnavailable,
					Headers: map[string][]string{"Content-Type": {"text/plain"}},
					Body:    "c2VydmljZSBidXN5", // base64("service busy")
				})
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				c.handleRequest(connCtx, frame.ID, payload)
			}()

		case FramePing:
			c.sendFrame(FramePong, frame.ID, nil)

		default:
			c.logger.Debug("relay: ignoring frame", "type", frame.Type)
		}
	}
}

// authenticate performs the Ed25519 challenge-response handshake.
func (c *Client) authenticate(ctx context.Context, conn *websocket.Conn) error {
	// Read challenge.
	_, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading challenge: %w", err)
	}

	var challenge struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(data, &challenge); err != nil {
		return fmt.Errorf("parsing challenge: %w", err)
	}

	// Sign challenge.
	sig := ed25519.Sign(c.privateKey, []byte(challenge.Challenge))

	resp, _ := json.Marshal(map[string]string{
		"signature": fmt.Sprintf("%x", sig),
	})
	if err := conn.Write(ctx, websocket.MessageText, resp); err != nil {
		return fmt.Errorf("sending signature: %w", err)
	}

	return nil
}

// sendFrame writes a frame to the WebSocket connection with a write timeout.
// Returns an error if the write fails or the connection is not available.
func (c *Client) sendFrame(typ FrameType, id string, payload any) error {
	var payloadBytes json.RawMessage
	if payload != nil {
		payloadBytes, _ = json.Marshal(payload)
	}

	frame := Frame{
		Type:    typ,
		ID:      id,
		Payload: payloadBytes,
	}
	data, _ := json.Marshal(frame)

	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return errors.New("not connected")
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	err := conn.Write(writeCtx, websocket.MessageText, data)
	c.mu.Unlock()
	cancel()
	return err
}

// sendResponse sends an HTTP response frame back to the relay.
func (c *Client) sendResponse(id string, resp HTTPResponsePayload) {
	if err := c.sendFrame(FrameHTTPResponse, id, resp); err != nil {
		c.logger.Debug("relay: failed to send response", "id", id, "err", err)
	}
}

// isAuthError returns true if the error originated from the authentication phase.
func isAuthError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "relay auth:")
}

// applyJitter adds ±25% random jitter to a duration.
func applyJitter(d time.Duration) time.Duration {
	jitter := float64(d) * 0.25
	offset := (rand.Float64()*2 - 1) * jitter
	return time.Duration(float64(d) + offset)
}
