package relay

import (
	"encoding/json"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
	}{
		{
			name: "http_request",
			frame: Frame{
				Type: FrameHTTPRequest,
				ID:   "req-1",
				Payload: mustMarshal(HTTPRequestPayload{
					Method:  "POST",
					Path:    "/api/gateway/request",
					Headers: map[string][]string{"Authorization": {"Bearer tok"}},
					Body:    "eyJ0ZXN0IjogdHJ1ZX0=",
				}),
			},
		},
		{
			name: "http_response",
			frame: Frame{
				Type: FrameHTTPResponse,
				ID:   "req-1",
				Payload: mustMarshal(HTTPResponsePayload{
					Status:  200,
					Headers: map[string][]string{"Content-Type": {"application/json"}},
					Body:    "eyJvayI6IHRydWV9",
				}),
			},
		},
		{
			name: "ping",
			frame: Frame{
				Type: FramePing,
				ID:   "ping-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.frame)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got Frame
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got.Type != tt.frame.Type {
				t.Errorf("type: got %q, want %q", got.Type, tt.frame.Type)
			}
			if got.ID != tt.frame.ID {
				t.Errorf("id: got %q, want %q", got.ID, tt.frame.ID)
			}
		})
	}
}

func TestHTTPRequestPayloadRoundTrip(t *testing.T) {
	orig := HTTPRequestPayload{
		Method:  "POST",
		Path:    "/api/gateway/request?foo=bar",
		Headers: map[string][]string{"X-Custom": {"a", "b"}},
		Body:    "dGVzdA==",
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got HTTPRequestPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Method != orig.Method {
		t.Errorf("method: got %q, want %q", got.Method, orig.Method)
	}
	if got.Path != orig.Path {
		t.Errorf("path: got %q, want %q", got.Path, orig.Path)
	}
	if got.Body != orig.Body {
		t.Errorf("body: got %q, want %q", got.Body, orig.Body)
	}
	if len(got.Headers["X-Custom"]) != 2 {
		t.Errorf("headers: got %d values, want 2", len(got.Headers["X-Custom"]))
	}
}

func TestHTTPResponsePayloadRoundTrip(t *testing.T) {
	orig := HTTPResponsePayload{
		Status:  201,
		Headers: map[string][]string{"Set-Cookie": {"a=1", "b=2"}},
		Body:    "cmVzcG9uc2U=",
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got HTTPResponsePayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Status != orig.Status {
		t.Errorf("status: got %d, want %d", got.Status, orig.Status)
	}
	if got.Body != orig.Body {
		t.Errorf("body: got %q, want %q", got.Body, orig.Body)
	}
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
