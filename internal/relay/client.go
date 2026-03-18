package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/clawvisor/pkg/config"
)

// Client manages a persistent WebSocket connection to the cloud relay.
type Client struct {
	relayURL   string
	daemonID   string
	privateKey ed25519.PrivateKey
	handler    http.Handler
	logger     *slog.Logger

	baseDelay time.Duration
	maxDelay  time.Duration

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
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
		relayURL:   cfg.URL,
		daemonID:   cfg.DaemonID,
		privateKey: key,
		handler:    handler,
		logger:     logger,
		baseDelay:  baseDelay,
		maxDelay:   maxDelay,
	}
}

// SetHandler replaces the HTTP handler used to dispatch relay requests.
// Must be called before Run.
func (c *Client) SetHandler(h http.Handler) {
	c.handler = h
}

// Run connects to the relay and blocks until ctx is cancelled.
// Handles reconnection with exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	delay := c.baseDelay

	for {
		connectedAt, err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
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
	return fmt.Sprintf("https://relay.clawvisor.com/d/%s", c.daemonID)
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
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Authenticate via challenge-response.
	if authErr := c.authenticate(ctx, conn); authErr != nil {
		return time.Time{}, fmt.Errorf("relay auth: %w", authErr)
	}

	connectedAt = time.Now()

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.connected = false
		c.mu.Unlock()
	}()

	c.logger.Info("relay connected", "daemon_id", c.daemonID)

	// Read loop.
	for {
		_, data, readErr := conn.Read(ctx)
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
			go c.handleRequest(ctx, frame.ID, payload)

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

// sendFrame writes a frame to the WebSocket connection. The write is
// serialized by the mutex to prevent concurrent writes from corrupting
// the WebSocket stream.
func (c *Client) sendFrame(typ FrameType, id string, payload any) {
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
		return
	}
	_ = conn.Write(context.Background(), websocket.MessageText, data)
	c.mu.Unlock()
}

// sendResponse sends an HTTP response frame back to the relay.
func (c *Client) sendResponse(id string, resp HTTPResponsePayload) {
	c.sendFrame(FrameHTTPResponse, id, resp)
}

// applyJitter adds ±25% random jitter to a duration.
func applyJitter(d time.Duration) time.Duration {
	jitter := float64(d) * 0.25
	offset := (rand.Float64()*2 - 1) * jitter
	return time.Duration(float64(d) + offset)
}
