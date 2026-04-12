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

	mu       sync.Mutex
	conn     *websocket.Conn
	connStop context.CancelFunc // cancelled when the connection drops

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

		err := c.connectOnce()
		if err != nil {
			if isAuthFailure(err) {
				c.logger.Warn("tunnel: auth failure, clearing pairing state")
				if c.onAuthFailure != nil {
					c.onAuthFailure()
				}
				return
			}
			delay := backoff.Next()
			c.logger.Warn("tunnel: disconnected, reconnecting", "err", err, "delay", delay)
			select {
			case <-time.After(delay):
			case <-c.ctx.Done():
				return
			}
		} else {
			backoff.Reset()
		}
	}
}

func (c *Client) connectOnce() error {
	u, err := url.Parse(c.cloudOrigin)
	if err != nil {
		return fmt.Errorf("parsing cloud origin: %w", err)
	}
	u.Scheme = "wss"
	u.Path = "/ws/daemon"

	conn, _, err := websocket.Dial(c.ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{},
	})
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
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
	if err := c.sendAuth(connCtx, conn); err != nil {
		return err
	}

	// Send capabilities frame.
	if err := c.sendCapabilities(connCtx, conn); err != nil {
		return err
	}

	// Start ping loop tied to this connection's context.
	go c.pingLoop(connCtx, connCancel)

	// Read loop.
	return c.readLoop(connCtx, conn)
}

func (c *Client) sendAuth(ctx context.Context, conn *websocket.Conn) error {
	payload, err := json.Marshal(AuthPayload{
		ConnectionToken: c.connectionToken,
	})
	if err != nil {
		return fmt.Errorf("marshalling auth: %w", err)
	}
	frame := Frame{
		Type:    FrameAuth,
		ID:      "auth",
		Payload: payload,
	}
	return c.writeJSON(ctx, conn, frame)
}

func (c *Client) sendCapabilities(ctx context.Context, conn *websocket.Conn) error {
	caps := c.buildCapabilities()
	payload, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("marshalling capabilities: %w", err)
	}
	frame := Frame{
		Type:    FrameCapabilities,
		ID:      generateFrameID(),
		Payload: payload,
	}
	return c.writeJSON(ctx, conn, frame)
}

// SendCapabilitiesUpdate sends an updated capabilities frame (e.g., after reload).
func (c *Client) SendCapabilitiesUpdate() error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	caps := c.buildCapabilities()
	payload, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("marshalling capabilities: %w", err)
	}
	frame := Frame{
		Type:    FrameCapabilities,
		ID:      generateFrameID(),
		Payload: payload,
	}
	return c.writeJSONLocked(frame)
}

// SendResponse sends a response frame for a given request.
func (c *Client) SendResponse(requestID string, resp *ResponsePayload) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshalling response: %w", err)
	}
	frame := Frame{
		Type:    FrameResponse,
		ID:      requestID,
		Payload: payload,
	}
	return c.writeJSONLocked(frame)
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
			payload, _ := json.Marshal(map[string]string{})
			frame := Frame{
				Type:    FramePing,
				ID:      generateFrameID(),
				Payload: payload,
			}
			if err := c.writeJSONLocked(frame); err != nil {
				c.logger.Warn("tunnel: ping write failed, closing connection", "err", err)
				connCancel()
				return
			}
		}
	}
}

// writeJSON writes a frame to a specific connection with a write timeout.
// Used during connection setup when we have a direct reference to the conn.
func (c *Client) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling frame: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

// writeJSONLocked writes a frame using the current connection under lock.
// Used by SendResponse, SendCapabilitiesUpdate, and pingLoop.
func (c *Client) writeJSONLocked(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling frame: %w", err)
	}

	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	writeErr := conn.Write(writeCtx, websocket.MessageText, data)
	c.mu.Unlock()
	cancel()
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
