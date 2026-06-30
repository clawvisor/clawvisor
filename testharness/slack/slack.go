// Package slack provides a minimal Slack Web API mock for E2E tests.
package slack

import (
	"testing"

	"github.com/clawvisor/clawvisor/testharness/httpmock"
)

type Mock struct {
	*httpmock.Server
}

func NewMock(t *testing.T) *Mock {
	t.Helper()
	srv := httpmock.New(t, map[string]httpmock.Response{
		"GET /api/conversations.list": {Body: map[string]any{
			"ok":       true,
			"channels": []map[string]any{},
		}},
		"GET /api/conversations.history": {Body: map[string]any{
			"ok":       true,
			"messages": []map[string]any{},
		}},
		"POST /api/chat.postMessage": {Body: map[string]any{
			"ok":      true,
			"ts":      "1700000000.000100",
			"channel": "C-MOCK",
		}},
		"GET /api/users.list": {Body: map[string]any{
			"ok":      true,
			"members": []map[string]any{},
		}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"SLACK_API_BASE_URL": m.URL()}
}
