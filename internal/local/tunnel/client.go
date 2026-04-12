package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

const (
	pingInterval = 30 * time.Second
	readTimeout  = 90 * time.Second
	writeTimeout = 10 * time.Second
	maxReadSize  = 1 << 20 // 1 MB

	// stableConnectionThreshold is how long a connection must stay alive
	// before backoff is reset on disconnect.
	stableConnectionThreshold = 60 * time.Second
)

// Client manages the WebSocket connection to the cloud.
type Client struct {
	cloudOrigin     string
	connectionToken string
	daemonName      string
	version         string
	registry        *services.Registry
	onRequest       func(ctx context.Context, id string, req *RequestPayload)
	onAuthFailure   func()
	logger          *slog.Logger

	// mu protects conn and connStop for reading. Writes to the WebSocket use
	// writeMu to avoid blocking readers during slow writes.
	mu       sync.Mutex
	conn     *websocket.Conn
	connStop context.CancelFunc

	// writeMu serializes WebSocket writes. Held only during conn.Write, never
	// during conn reads or status checks, so IsConnected/Close stay responsive.
	writeMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
}

// ClientConfig holds configuration for the tunnel client.
type ClientConfig struct {
	CloudOrigin     string
	ConnectionToken string
	DaemonName      string
	Version         string
	Registry        *services.Registry
	OnRequest       func(ctx context.Context, id string, req *RequestPayload)
	OnAuthFailure   func()
	Logger          *slog.Logger
}

// NewClient creates a new tunnel client.
func NewClient(cfg ClientConfig) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cloudOrigin:     cfg.CloudOrigin,
		connectionToken: cfg.ConnectionToken,
		daemonName:      cfg.DaemonName,
		version:         cfg.Version,
		registry:        cfg.Registry,
		onRequest:       cfg.OnRequest,
		onAuthFailure:   cfg.OnAuthFailure,
		logger:          logger,
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Connect establishes and maintains the WebSocket connection with automatic reconnection.
func (c *Client) Connect() {
	backoff := NewBackoff()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		connectedAt, err := c.connectOnce()
		if err != nil {
			if isAuthFailure(err) {
				c.logger.Warn("tunnel: auth failure, clearing pairing state")
				if c.onAuthFailure != nil {
					c.onAuthFailure()
				}
				return
			}

			// Reset backoff if the connection was stable before dropping.
			if !connectedAt.IsZero() && time.Since(connectedAt) > stableConnectionThreshold {
				backoff.Reset()
			}

			delay := backoff.Next()
			c.logger.Warn("tunnel: disconnected, reconnecting", "err", err, "delay", delay)
			select {
			case <-time.After(delay):
			case <-c.ctx.Done():
				return
			}
		}
	}
}

// connectOnce returns (connectedAt, error). connectedAt is set once the
// connection is authenticated and serving; it is zero if dial/auth failed.
func (c *Client) connectOnce() (time.Time, error) {
	u, err := url.Parse(c.cloudOrigin)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cloud origin: %w", err)
	}
	u.Scheme = "wss"
	u.Path = "/ws/daemon"

	conn, _, err := websocket.Dial(c.ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("websocket dial: %w", err)
	}
	conn.SetReadLimit(maxReadSize)

	// Create a connection-scoped context that is cancelled when this connection drops.
	connCtx, connCancel := context.WithCancel(c.ctx)

	c.mu.Lock()
	c.conn = conn
	c.connStop = connCancel
	c.mu.Unlock()

	defer func() {
		connCancel()
		c.mu.Lock()
		c.conn = nil
		c.connStop = nil
		c.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Send auth frame.
	if err := c.writeToConn(connCtx, conn, FrameAuth, "auth", AuthPayload{
		ConnectionToken: c.connectionToken,
	}); err != nil {
		return time.Time{}, fmt.Errorf("sending auth: %w", err)
	}

	// Send capabilities frame.
	caps := c.buildCapabilities()
	if err := c.writeToConn(connCtx, conn, FrameCapabilities, generateFrameID(), caps); err != nil {
		return time.Time{}, fmt.Errorf("sending capabilities: %w", err)
	}

	connectedAt := time.Now()

	// Start ping loop tied to this connection's context.
	go c.pingLoop(connCtx, connCancel)

	// Read loop.
	readErr := c.readLoop(connCtx, conn)
	return connectedAt, readErr
}

// SendCapabilitiesUpdate sends an updated capabilities frame (e.g., after reload).
func (c *Client) SendCapabilitiesUpdate() error {
	caps := c.buildCapabilities()
	return c.writeFrame(FrameCapabilities, generateFrameID(), caps)
}

// SendResponse sends a response frame for a given request.
func (c *Client) SendResponse(requestID string, resp *ResponsePayload) error {
	return c.writeFrame(FrameResponse, requestID, resp)
}

// Close disconnects the client.
func (c *Client) Close() {
	c.cancel()
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "shutdown")
	}
	c.mu.Unlock()
}

// IsConnected returns whether the WebSocket is currently connected.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) readLoop(connCtx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, readCancel := context.WithTimeout(connCtx, readTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			return fmt.Errorf("read error: %w", err)
		}

		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			c.logger.Warn("tunnel: invalid frame", "err", err)
			continue
		}

		switch frame.Type {
		case FrameRequest:
			var req RequestPayload
			if err := json.Unmarshal(frame.Payload, &req); err != nil {
				c.logger.Warn("tunnel: invalid request payload", "err", err)
				continue
			}
			if c.onRequest != nil {
				go c.onRequest(connCtx, frame.ID, &req)
			}

		case FramePong:
			// Keepalive acknowledged — no action needed, read timeout is
			// reset on each successful Read call.

		default:
			c.logger.Debug("tunnel: unknown frame type", "type", frame.Type)
		}
	}
}

func (c *Client) pingLoop(connCtx context.Context, connCancel context.CancelFunc) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-connCtx.Done():
			return
		case <-ticker.C:
			if err := c.writeFrame(FramePing, generateFrameID(), map[string]string{}); err != nil {
				c.logger.Warn("tunnel: ping write failed, closing connection", "err", err)
				connCancel()
				return
			}
		}
	}
}

// writeToConn writes a frame to a specific connection with a write timeout.
// Used during connection setup when we have a direct reference to the conn
// and no other goroutines are writing yet.
func (c *Client) writeToConn(ctx context.Context, conn *websocket.Conn, typ FrameType, id string, payload any) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	frame := Frame{Type: typ, ID: id, Payload: payloadBytes}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshalling frame: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

// writeFrame grabs the current connection under mu, then writes under writeMu.
// mu is NOT held during the write itself, so IsConnected/Close remain responsive.
func (c *Client) writeFrame(typ FrameType, id string, payload any) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	frame := Frame{Type: typ, ID: id, Payload: payloadBytes}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshalling frame: %w", err)
	}

	// Grab conn reference under mu (fast).
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	// Serialize writes under writeMu (potentially slow).
	c.writeMu.Lock()
	writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	writeErr := conn.Write(writeCtx, websocket.MessageText, data)
	cancel()
	c.writeMu.Unlock()

	return writeErr
}

func (c *Client) buildCapabilities() CapabilitiesPayload {
	svcs := c.registry.All()
	caps := CapabilitiesPayload{
		Version:  c.version,
		Name:     c.daemonName,
		Services: make([]ServiceCapability, 0, len(svcs)),
	}

	for _, svc := range svcs {
		sc := ServiceCapability{
			ID:          svc.ID,
			Name:        svc.Name,
			Description: svc.Description,
			Icon:        svc.Icon,
			Actions:     make([]ActionCapability, 0, len(svc.Actions)),
		}
		for _, a := range svc.Actions {
			ac := ActionCapability{
				ID:          a.ID,
				Name:        a.Name,
				Description: a.Description,
				Params:      make([]ParamCapability, 0, len(a.Params)),
			}
			for _, p := range a.Params {
				ac.Params = append(ac.Params, ParamCapability{
					Name:        p.Name,
					Type:        p.Type,
					Required:    p.Required,
					Description: p.Description,
				})
			}
			sc.Actions = append(sc.Actions, ac)
		}
		caps.Services = append(caps.Services, sc)
	}

	return caps
}

func isAuthFailure(err error) bool {
	// coder/websocket wraps close frames in its error messages.
	// Auth failure is signaled by close code 4001 from the cloud.
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "4001")
}

func generateFrameID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
