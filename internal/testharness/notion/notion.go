// Package notion provides a minimal Notion API mock for E2E tests.
package notion

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/testharness/httpmock"
)

type Mock struct{ *httpmock.Server }

func NewMock(t *testing.T) *Mock {
	t.Helper()
	srv := httpmock.New(t, map[string]httpmock.Response{
		"POST /v1/search":                    {Body: map[string]any{"object": "list", "results": []any{}}},
		"GET /v1/pages/{id}":                 {Body: map[string]any{"object": "page", "id": "mock"}},
		"POST /v1/pages":                     {Body: map[string]any{"object": "page", "id": "mock-created"}},
		"PATCH /v1/pages/{id}":               {Body: map[string]any{"object": "page", "id": "mock"}},
		"POST /v1/databases/{id}/query":      {Body: map[string]any{"object": "list", "results": []any{}}},
		"GET /v1/databases":                  {Body: map[string]any{"object": "list", "results": []any{}}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"NOTION_API_BASE_URL": m.URL()}
}
