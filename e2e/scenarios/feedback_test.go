package scenarios_test

import (
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestFeedbackReport — agents POST bug reports; user-side reads them.
func TestFeedbackReport(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	// Create an agent so we have a valid agent token to submit feedback.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "feedback-agent"}, &agent)

	// Agent submits a bug report.
	resp := cvDo(t, cv, agent.Token, "POST", "/api/feedback/report", map[string]any{
		"category":    "ui",
		"severity":    "medium",
		"description": "buttons unclickable in dark mode",
	})
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("feedback report status=%d", resp.StatusCode)
	}
}

// TestNPSSubmit — agent submits an NPS rating.
func TestNPSSubmit(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "nps-agent"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/feedback/nps", map[string]any{
		"score":   9,
		"comment": "nice tool",
	})
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("nps status=%d", resp.StatusCode)
	}
}
