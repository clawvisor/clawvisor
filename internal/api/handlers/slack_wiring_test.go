package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// Slack must stay reachable on deployments without Redis. The Redis-backed
// OAuth state store is nil there, and an earlier version of the wiring
// treated that as "Slack unavailable" — silently disabling the feature on
// every single-instance deployment, including local dev.
func TestSetSlack_NilStateStoreKeepsInMemoryDefault(t *testing.T) {
	h := NewNotificationsHandler(nil, nil, nil, nil, nil, nil, nil, "http://localhost")
	if h.oauthState == nil {
		t.Fatal("constructor left oauthState nil; Slack install would panic")
	}
	def := h.oauthState

	h.SetSlack(stubSlackCfg{}, stubSlackInstaller{}, nil)
	if h.oauthState == nil {
		t.Fatal("SetSlack(nil) cleared the default state store")
	}
	if h.oauthState != def {
		t.Fatal("SetSlack(nil) replaced the default state store")
	}

	// With both Slack deps present the routes must answer, not 501.
	rec := httptest.NewRecorder()
	h.SlackChannels(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/slack/channels", nil))
	if rec.Code == http.StatusNotImplemented {
		t.Fatal("Slack routes reported disabled despite being wired")
	}
}

// Without Slack credentials the routes must report disabled rather than
// panicking on nil dependencies.
func TestSlackRoutesDisabledWithoutCredentials(t *testing.T) {
	h := NewNotificationsHandler(nil, nil, nil, nil, nil, nil, nil, "http://localhost")
	rec := httptest.NewRecorder()
	h.SlackChannels(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/slack/channels", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501 when Slack is not configured", rec.Code)
	}
}

type stubSlackCfg struct{}

func (stubSlackCfg) SaveSlackConfig(context.Context, string, notify.SlackConfig) error { return nil }
func (stubSlackCfg) SlackConfig(context.Context, string) (notify.SlackConfig, error) {
	return notify.SlackConfig{}, nil
}
func (stubSlackCfg) DeleteSlackConfig(context.Context, string) error { return nil }

type stubSlackInstaller struct{}

func (stubSlackInstaller) SlackInstallURL(string) (string, error) {
	return "https://slack.example", nil
}
func (stubSlackInstaller) CompleteSlackInstall(context.Context, string) (notify.SlackInstall, error) {
	return notify.SlackInstall{}, nil
}
func (stubSlackInstaller) ListSlackChannels(context.Context, string) ([]notify.SlackChannel, error) {
	return nil, nil
}
func (stubSlackInstaller) LookupSlackUser(context.Context, string, string) (string, error) {
	return "", nil
}
