package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/tui/client"
)

type fakeRuntimeControlsClient struct {
	decision             *client.RuntimePresetDecision
	settings             *client.AgentRuntimeSettings
	appliedProfiles      []string
	updatedSettings      []client.AgentRuntimeSettings
	upsertedDecisions    []client.RuntimePresetDecision
	getPresetCalls       int
	getSettingsCalls     int
	applyErr             error
	updateSettingsErr    error
	upsertDecisionErr    error
}

func (f *fakeRuntimeControlsClient) GetRuntimePresetDecision(commandKey, profile string) (*client.RuntimePresetDecision, error) {
	f.getPresetCalls++
	return f.decision, nil
}

func (f *fakeRuntimeControlsClient) GetAgentRuntimeSettings(agentID string) (*client.AgentRuntimeSettings, error) {
	f.getSettingsCalls++
	if f.settings == nil {
		return nil, nil
	}
	cp := *f.settings
	return &cp, nil
}

func (f *fakeRuntimeControlsClient) ApplyRuntimeStarterProfile(profileID, agentID string) ([]client.RuntimePolicyRule, error) {
	f.appliedProfiles = append(f.appliedProfiles, profileID+":"+agentID)
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return nil, nil
}

func (f *fakeRuntimeControlsClient) UpdateAgentRuntimeSettings(agentID string, settings client.AgentRuntimeSettings) (*client.AgentRuntimeSettings, error) {
	f.updatedSettings = append(f.updatedSettings, settings)
	if f.updateSettingsErr != nil {
		return nil, f.updateSettingsErr
	}
	cp := settings
	return &cp, nil
}

func (f *fakeRuntimeControlsClient) UpsertRuntimePresetDecision(decision client.RuntimePresetDecision) (*client.RuntimePresetDecision, error) {
	f.upsertedDecisions = append(f.upsertedDecisions, decision)
	if f.upsertDecisionErr != nil {
		return nil, f.upsertDecisionErr
	}
	cp := decision
	return &cp, nil
}

func withRuntimeControlsTestSeams(t *testing.T, cl runtimeControlsClient, stdin io.Reader, stderr io.Writer, tty bool) {
	t.Helper()
	prevNewClient := newRuntimeControlsClient
	prevStdin := runtimeControlsStdin
	prevStderr := runtimeControlsStderr
	prevTTYCheck := runtimeControlsTTYCheck
	prevOverride := runtimeProfileOverride
	newRuntimeControlsClient = func() (runtimeControlsClient, error) { return cl, nil }
	runtimeControlsStdin = stdin
	runtimeControlsStderr = stderr
	runtimeControlsTTYCheck = func() bool { return tty }
	runtimeProfileOverride = ""
	t.Cleanup(func() {
		newRuntimeControlsClient = prevNewClient
		runtimeControlsStdin = prevStdin
		runtimeControlsStderr = prevStderr
		runtimeControlsTTYCheck = prevTTYCheck
		runtimeProfileOverride = prevOverride
	})
}

func TestShouldSuppressStarterProfilePrompt(t *testing.T) {
	tests := []struct {
		name     string
		decision *client.RuntimePresetDecision
		settings *client.AgentRuntimeSettings
		profile  string
		want     bool
	}{
		{
			name: "suppresses skipped decision",
			decision: &client.RuntimePresetDecision{
				Decision: "skipped",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses always skip decision",
			decision: &client.RuntimePresetDecision{
				Decision: "always_skip",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses applied decision",
			decision: &client.RuntimePresetDecision{
				Decision: "applied",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "suppresses matching starter profile in settings",
			settings: &client.AgentRuntimeSettings{
				StarterProfile: "Codex",
			},
			profile: "codex",
			want:    true,
		},
		{
			name: "does not suppress unrelated state",
			decision: &client.RuntimePresetDecision{
				Decision: "unknown",
			},
			settings: &client.AgentRuntimeSettings{
				StarterProfile: "claude",
			},
			profile: "codex",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSuppressStarterProfilePrompt(tt.decision, tt.settings, tt.profile); got != tt.want {
				t.Fatalf("shouldSuppressStarterProfilePrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveModeNotice(t *testing.T) {
	got := observeModeNotice()
	want := "Clawvisor is in observe mode for this session. Actions are being analyzed and logged, but not blocked. To remove this notice, switch this agent to enforce mode in the Clawvisor dashboard."
	if got != want {
		t.Fatalf("observeModeNotice() = %q, want %q", got, want)
	}
}

func TestMaybeOfferStarterProfile(t *testing.T) {
	tests := []struct {
		name                    string
		decision                *client.RuntimePresetDecision
		settings                *client.AgentRuntimeSettings
		input                   string
		wantApply               bool
		wantStarterProfile      string
		wantPresetDecisions     []string
		wantPrompt              bool
		wantAppliedMessage      bool
	}{
		{
			name: "suppresses previously skipped decision",
			decision: &client.RuntimePresetDecision{
				Decision: "skipped",
			},
		},
		{
			name: "suppresses matching starter profile in settings",
			settings: &client.AgentRuntimeSettings{
				StarterProfile: "codex",
			},
		},
		{
			name:                "applies starter profile on yes",
			settings:            &client.AgentRuntimeSettings{AgentID: "agent-123"},
			input:               "y\n",
			wantApply:           true,
			wantStarterProfile:  "codex",
			wantPresetDecisions: []string{"applied"},
			wantPrompt:          true,
			wantAppliedMessage:  true,
		},
		{
			name:                "persists skipped decision on no",
			settings:            &client.AgentRuntimeSettings{AgentID: "agent-123"},
			input:               "n\n",
			wantPresetDecisions: []string{"skipped"},
			wantPrompt:          true,
		},
		{
			name:                "persists always skip decision",
			settings:            &client.AgentRuntimeSettings{AgentID: "agent-123"},
			input:               "a\n",
			wantPresetDecisions: []string{"always_skip"},
			wantPrompt:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := &fakeRuntimeControlsClient{
				decision: tt.decision,
				settings: tt.settings,
			}
			var stderr bytes.Buffer
			withRuntimeControlsTestSeams(t, fakeClient, strings.NewReader(tt.input), &stderr, true)

			if err := maybeOfferStarterProfile(&resolvedAgentCredentials{AgentID: "agent-123"}, []string{"codex"}); err != nil {
				t.Fatalf("maybeOfferStarterProfile() error = %v", err)
			}

			promptShown := strings.Contains(stderr.String(), "Apply recommended runtime rules for Codex?")
			if promptShown != tt.wantPrompt {
				t.Fatalf("prompt shown = %v, want %v; stderr=%q", promptShown, tt.wantPrompt, stderr.String())
			}
			appliedMessageShown := strings.Contains(stderr.String(), "Applied Codex starter profile.")
			if appliedMessageShown != tt.wantAppliedMessage {
				t.Fatalf("applied message shown = %v, want %v; stderr=%q", appliedMessageShown, tt.wantAppliedMessage, stderr.String())
			}

			if got := len(fakeClient.appliedProfiles) > 0; got != tt.wantApply {
				t.Fatalf("starter profile applied = %v, want %v", got, tt.wantApply)
			}
			if tt.wantStarterProfile != "" {
				if len(fakeClient.updatedSettings) != 1 {
					t.Fatalf("expected one settings update, got %+v", fakeClient.updatedSettings)
				}
				if got := fakeClient.updatedSettings[0].StarterProfile; got != tt.wantStarterProfile {
					t.Fatalf("updated starter profile = %q, want %q", got, tt.wantStarterProfile)
				}
			} else if len(fakeClient.updatedSettings) != 0 {
				t.Fatalf("unexpected settings updates: %+v", fakeClient.updatedSettings)
			}

			gotDecisions := make([]string, 0, len(fakeClient.upsertedDecisions))
			for _, decision := range fakeClient.upsertedDecisions {
				gotDecisions = append(gotDecisions, decision.Decision)
				if decision.CommandKey != "codex" {
					t.Fatalf("decision command key = %q, want codex", decision.CommandKey)
				}
				if decision.Profile != "codex" {
					t.Fatalf("decision profile = %q, want codex", decision.Profile)
				}
			}
			if strings.Join(gotDecisions, ",") != strings.Join(tt.wantPresetDecisions, ",") {
				t.Fatalf("preset decisions = %v, want %v", gotDecisions, tt.wantPresetDecisions)
			}
		})
	}
}
