package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
)

type contextKey string

const viaRelayKey contextKey = "clawvisor-via-relay"

// ViaRelay returns true if the request arrived through the relay tunnel.
func ViaRelay(ctx context.Context) bool {
	v, _ := ctx.Value(viaRelayKey).(bool)
	return v
}

// WithViaRelay returns a context marked as relay-originated. Exported for
// testing E2E middleware in other packages.
func WithViaRelay(ctx context.Context) context.Context {
	return context.WithValue(ctx, viaRelayKey, true)
}

// handleRequest processes a single proxied HTTP request from the relay.
func (c *Client) handleRequest(ctx context.Context, id string, payload HTTPRequestPayload) {
	body, _ := base64.StdEncoding.DecodeString(payload.Body)

	tunnelCtx := context.WithValue(ctx, viaRelayKey, true)
	req, err := http.NewRequestWithContext(tunnelCtx, payload.Method, payload.Path, bytes.NewReader(body))
	if err != nil {
		c.logger.Warn("relay: failed to build request", "id", id, "err", err)
		c.sendResponse(id, HTTPResponsePayload{
			Status:  http.StatusBadGateway,
			Headers: map[string][]string{"Content-Type": {"text/plain"}},
			Body:    base64.StdEncoding.EncodeToString([]byte("failed to construct request")),
		})
		return
	}
	for k, vals := range payload.Headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	resp := HTTPResponsePayload{
		Status:  rec.Code,
		Headers: rec.Header(),
		Body:    base64.StdEncoding.EncodeToString(rec.Body.Bytes()),
	}
	c.sendResponse(id, resp)
}
