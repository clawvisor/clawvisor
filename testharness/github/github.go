// Package github provides a minimal in-memory GitHub REST mock for E2E
// tests. Implements only the routes the GitHub adapter calls.
package github

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
		// List issues in a repo.
		"GET /repos/{owner}/{repo}/issues": {Body: []map[string]any{}},
		// Get a single issue.
		"GET /repos/{owner}/{repo}/issues/{num}": {Body: map[string]any{
			"id": 1, "number": 1, "title": "mock issue", "state": "open",
		}},
		// Create an issue.
		"POST /repos/{owner}/{repo}/issues": {Status: 201, Body: map[string]any{
			"id": 999, "number": 999, "html_url": "https://github.com/mock/issue/999",
		}},
		// List PRs.
		"GET /repos/{owner}/{repo}/pulls": {Body: []map[string]any{}},
		// List user repos.
		"GET /user/repos": {Body: []map[string]any{}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"GITHUB_API_BASE_URL": m.URL()}
}
