// Package twilio provides a minimal Twilio API mock for E2E tests.
package twilio

import (
	"testing"

	"github.com/clawvisor/clawvisor/testharness/httpmock"
)

type Mock struct{ *httpmock.Server }

func NewMock(t *testing.T) *Mock {
	t.Helper()
	srv := httpmock.New(t, map[string]httpmock.Response{
		"POST /2010-04-01/Accounts/{acct}/Messages.json": {
			Status: 201,
			Body: map[string]any{
				"sid": "SM" + "mock", "status": "queued", "to": "+15555555555",
			},
		},
		"GET /2010-04-01/Accounts/{acct}/Messages.json": {Body: map[string]any{
			"messages": []any{}, "first_page_uri": "",
		}},
	})
	return &Mock{Server: srv}
}

func (m *Mock) Env() map[string]string {
	return map[string]string{"TWILIO_API_BASE_URL": m.URL()}
}
