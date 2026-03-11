package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SSEEvent represents an event received from the SSE stream.
type SSEEvent struct {
	Type string `json:"type"` // "queue", "tasks", "audit"
	ID   string `json:"id"`   // optional task/audit ID
}

// GetEventTicket fetches a short-lived SSE ticket.
func (c *Client) GetEventTicket() (string, error) {
	var resp struct {
		Ticket string `json:"ticket"`
	}
	if err := c.post("/api/events/ticket", nil, &resp); err != nil {
		return "", err
	}
	return resp.Ticket, nil
}

// SubscribeSSE opens an SSE connection and sends events to the channel.
// Blocks until the connection closes or ctx is cancelled.
func (c *Client) SubscribeSSE(ctx context.Context, ticket string, ch chan<- SSEEvent) error {
	url := c.baseURL + "/api/events?ticket=" + ticket

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// Use a client without the default 10s timeout — SSE connections are long-lived.
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("SSE returned %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	var eventType, data string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Blank line = end of event.
			if data != "" {
				var evt SSEEvent
				if json.Unmarshal([]byte(data), &evt) == nil {
					if eventType != "" {
						evt.Type = eventType
					}
					select {
					case ch <- evt:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				eventType = ""
				data = ""
			}
			continue
		}

		if strings.HasPrefix(line, ":") {
			// Comment / keepalive — ignore.
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("SSE read: %w", err)
	}
	return nil
}
