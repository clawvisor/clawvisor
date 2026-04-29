package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/clawvisor/clawvisor/internal/daemon"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

var runtimeProfileOverride string

func maybeOfferStarterProfile(creds *resolvedAgentCredentials, launchedArgs []string) error {
	if creds == nil || strings.TrimSpace(creds.AgentID) == "" {
		return nil
	}
	commandKey, profileID := runtimepolicy.DetectStarterProfile(runtimeProfileOverride, launchedArgs)
	if profileID == "" || commandKey == "" {
		return nil
	}
	if !isInteractiveTTY(os.Stdin) {
		return nil
	}
	cl, err := daemon.NewAPIClient()
	if err != nil {
		return nil
	}
	decision, err := cl.GetRuntimePresetDecision(commandKey, profileID)
	if err == nil && decision != nil {
		switch decision.Decision {
		case "always_skip", "applied":
			return nil
		}
	}
	settings, err := cl.GetAgentRuntimeSettings(creds.AgentID)
	if err != nil {
		return nil
	}
	profile, ok := runtimepolicy.StarterProfileByID(profileID)
	if !ok {
		return nil
	}
	if strings.EqualFold(settings.StarterProfile, profileID) {
		return nil
	}

	fmt.Fprintf(os.Stderr, "Apply recommended runtime rules for %s? [Y/n/a] ", profile.DisplayName)
	choice, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return nil
	}
	choice = strings.ToLower(strings.TrimSpace(choice))
	switch choice {
	case "", "y", "yes":
		if _, err := cl.ApplyRuntimeStarterProfile(profileID, creds.AgentID); err == nil {
			settings.StarterProfile = profileID
			_, _ = cl.UpdateAgentRuntimeSettings(creds.AgentID, *settings)
			_, _ = cl.UpsertRuntimePresetDecision(client.RuntimePresetDecision{
				CommandKey: commandKey,
				Profile:    profileID,
				Decision:   "applied",
			})
			fmt.Fprintf(os.Stderr, "Applied %s starter profile.\n", profile.DisplayName)
		}
	case "a", "always", "always-skip", "always_skip":
		_, _ = cl.UpsertRuntimePresetDecision(client.RuntimePresetDecision{
			CommandKey: commandKey,
			Profile:    profileID,
			Decision:   "always_skip",
		})
	case "n", "no", "skip":
		_, _ = cl.UpsertRuntimePresetDecision(client.RuntimePresetDecision{
			CommandKey: commandKey,
			Profile:    profileID,
			Decision:   "skipped",
		})
	}
	return nil
}

func printObserveModeNotice(observe bool) {
	if !observe {
		return
	}
	fmt.Fprintln(os.Stderr, "Clawvisor is in observe mode for this session. Actions are being analyzed and logged, but not blocked.")
}

func isInteractiveTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
