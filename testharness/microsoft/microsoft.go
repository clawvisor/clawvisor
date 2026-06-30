// Package microsoft provides a minimal Microsoft Graph API mock for E2E
// tests (Outlook + OneDrive).
package microsoft

import (
	"testing"

	"github.com/clawvisor/clawvisor/testharness/httpmock"
)

type Mock struct{ *httpmock.Server }

func NewMock(t *testing.T) *Mock {
	t.Helper()
	srv := httpmock.New(t, map[string]httpmock.Response{
		// Outlook messages.
		"GET /v1.0/me/messages":          {Body: map[string]any{"value": []any{}}},
		"GET /v1.0/me/messages/{id}":     {Body: map[string]any{"id": "mock"}},
		"POST /v1.0/me/sendMail":         {Status: 202},
		// Outlook calendar.
		"GET /v1.0/me/calendar/events":     {Body: map[string]any{"value": []any{}}},
		"POST /v1.0/me/calendar/events":    {Status: 201, Body: map[string]any{"id": "mock-event"}},
		// OneDrive.
		"GET /v1.0/me/drive/root/children": {Body: map[string]any{"value": []any{}}},
		"GET /v1.0/me/drive/items/{id}":    {Body: map[string]any{"id": "mock"}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"MICROSOFT_GRAPH_BASE_URL": m.URL()}
}
