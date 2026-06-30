// Package linear provides a minimal Linear GraphQL mock for E2E tests.
// Linear is GraphQL — all calls hit POST /graphql. We mock it as a single
// endpoint that returns a configurable response per test.
package linear

import (
	"testing"

	"github.com/clawvisor/clawvisor/testharness/httpmock"
)

type Mock struct{ *httpmock.Server }

func NewMock(t *testing.T) *Mock {
	t.Helper()
	srv := httpmock.New(t, map[string]httpmock.Response{
		"POST /graphql": {Body: map[string]any{"data": map[string]any{
			"issues": map[string]any{"nodes": []any{}},
		}}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"LINEAR_API_BASE_URL": m.URL()}
}
