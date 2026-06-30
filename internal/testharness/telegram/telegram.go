// Package telegram provides a minimal Telegram Bot API mock for E2E tests.
//
// Telegram URLs embed the bot token in the path (/bot<TOKEN>/method) which
// the Go 1.22 mux can't express as a path wildcard. We sidestep that by
// stripping the /bot<token>/ prefix in middleware and dispatching on the
// trailing method segment alone.
package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Mock struct {
	mu       sync.Mutex
	srv      *httptest.Server
	captured []Captured
}

type Captured struct {
	Method   string         // Telegram method: "sendMessage", "getMe", …
	HTTPVerb string         // POST / GET
	Body     map[string]any // parsed JSON body
}

func NewMock(t *testing.T) *Mock {
	t.Helper()
	m := &Mock{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *Mock) URL() string { return m.srv.URL }

func (m *Mock) Env() map[string]string {
	return map[string]string{"TELEGRAM_API_BASE_URL": m.srv.URL}
}

func (m *Mock) Reset() {
	m.mu.Lock()
	m.captured = nil
	m.mu.Unlock()
}

func (m *Mock) Captured() []Captured {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Captured, len(m.captured))
	copy(out, m.captured)
	return out
}

// AssertReceived fails the test if the given Telegram method wasn't called.
func (m *Mock) AssertReceived(t *testing.T, method string) Captured {
	t.Helper()
	for _, c := range m.Captured() {
		if c.Method == method {
			return c
		}
	}
	t.Fatalf("telegram: no call to method %q (captured %d)", method, len(m.Captured()))
	return Captured{}
}

func (m *Mock) handle(w http.ResponseWriter, r *http.Request) {
	// Path shape: /bot<TOKEN>/<method>
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	method := ""
	if len(parts) == 2 {
		method = parts[1]
	}

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()

	m.mu.Lock()
	m.captured = append(m.captured, Captured{
		Method:   method,
		HTTPVerb: r.Method,
		Body:     body,
	})
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "sendMessage":
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1700000000}}`))
	case "getMe":
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":12345,"is_bot":true,"username":"mock_bot"}}`))
	case "getUpdates":
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	case "getChat":
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":-1001234,"type":"group","title":"Mock"}}`))
	default:
		_, _ = w.Write([]byte(`{"ok":true,"result":null}`))
	}
}
