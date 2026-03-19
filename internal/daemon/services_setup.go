package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/browser"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/tui/client"
	"gopkg.in/yaml.v3"
)

// knownGoogleServices are always shown in the setup menu even when the server
// hasn't registered their adapters (which requires client_id/secret in config).
var knownGoogleServices = []client.ServiceInfo{
	{ID: "google.gmail", Name: display.ServiceName("google.gmail"), Description: display.ServiceDescription("google.gmail"), OAuth: true, RequiresActivation: true, Status: "not_activated"},
	{ID: "google.calendar", Name: display.ServiceName("google.calendar"), Description: display.ServiceDescription("google.calendar"), OAuth: true, RequiresActivation: true, Status: "not_activated"},
	{ID: "google.drive", Name: display.ServiceName("google.drive"), Description: display.ServiceDescription("google.drive"), OAuth: true, RequiresActivation: true, Status: "not_activated"},
	{ID: "google.contacts", Name: display.ServiceName("google.contacts"), Description: display.ServiceDescription("google.contacts"), OAuth: true, RequiresActivation: true, Status: "not_activated"},
}

const continueOption = "__continue__"

// runServiceSetup presents the interactive service-selection loop.
// It returns needsRestart=true when Google OAuth credentials were collected
// and the server must be restarted with the updated config.
func runServiceSetup(apiClient *client.Client, dataDir string) (needsRestart bool, err error) {
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Connect services"))
	fmt.Println(dim.Padding(0, 2).Render("Connect the services you want Clawvisor to manage."))
	fmt.Println()

	for {
		resp, err := apiClient.GetServices()
		if err != nil {
			return false, fmt.Errorf("fetching services: %w", err)
		}

		// Inject known Google services that the server doesn't list (because
		// their adapters aren't registered without client_id/secret in config).
		services := injectMissingGoogleServices(resp.Services)

		// If there are no services at all, confirm the user wants to skip.
		if len(services) == 0 {
			skip := false
			if err := huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title("No services are available. Continue without connecting?").
						Affirmative("Yes, continue").
						Negative("Go back").
						Value(&skip),
				),
			).Run(); err != nil || skip {
				return false, nil
			}
			return false, nil
		}

		selected, err := presentServiceMenu(services)
		if err != nil {
			if err == huh.ErrUserAborted {
				return needsRestart, nil
			}
			return false, err
		}

		if selected == continueOption {
			return needsRestart, nil
		}

		// Find the selected service (use composite key for multi-account).
		var svc client.ServiceInfo
		found := false
		for _, s := range services {
			if serviceKey(s) == selected {
				svc = s
				found = true
				break
			}
		}
		if !found {
			continue
		}

		if svc.Status == "activated" {
			if err := manageConnectedService(apiClient, svc, dataDir); err != nil && err != huh.ErrUserAborted {
				fmt.Printf("\n  %s\n\n", dim.Render(err.Error()))
			}
			continue
		}

		restart, err := activateService(apiClient, svc, dataDir)
		if err != nil {
			fmt.Printf("\n  %s\n\n", dim.Render("Could not connect: "+err.Error()))
			continue
		}
		if restart {
			// Google creds were written — must restart the server before
			// OAuth can proceed. The service setup loop will resume after restart.
			return true, nil
		}
	}
}

// serviceKey returns a unique key for a service entry, accounting for aliases.
func serviceKey(s client.ServiceInfo) string {
	if s.Alias != "" {
		return s.ID + ":" + s.Alias
	}
	return s.ID
}

// presentServiceMenu builds a flat huh.Select list with ✓/○ status icons,
// Google services first, then others, with "── Continue →" at the top.
func presentServiceMenu(services []client.ServiceInfo) (selected string, err error) {
	// Partition: Google first, then the rest.
	var google, other []client.ServiceInfo
	for _, s := range services {
		if strings.HasPrefix(s.ID, "google.") {
			google = append(google, s)
		} else {
			other = append(other, s)
		}
	}

	var opts []huh.Option[string]

	// Continue at the top.
	opts = append(opts, huh.NewOption(dim.Render("── Done connecting services →"), continueOption))

	// Google services first.
	for _, list := range [][]client.ServiceInfo{google, other} {
		for _, s := range list {
			opts = append(opts, huh.NewOption(serviceLabel(s), serviceKey(s)))
		}
	}

	var choice string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a service to connect").
				Options(opts...).
				Value(&choice),
		),
	).Run(); err != nil {
		return "", err
	}

	return choice, nil
}

// serviceLabel renders a service name with a green ✓ or gray ○ indicator.
func serviceLabel(s client.ServiceInfo) string {
	var icon string
	if s.Status == "activated" {
		icon = green.Render("✓")
	} else {
		icon = dim.Render("○")
	}
	label := fmt.Sprintf("%s  %s", icon, s.Name)
	if s.Alias != "" {
		label += dim.Render("  (" + s.Alias + ")")
	}
	return label
}

// activateService dispatches to the correct activation flow for the service.
func activateService(apiClient *client.Client, svc client.ServiceInfo, dataDir string) (needsRestart bool, err error) {
	switch {
	case svc.CredentialFree:
		return false, activateCredentialFreeService(apiClient, svc)
	case svc.OAuth:
		return activateOAuthService(apiClient, svc, dataDir)
	default:
		return false, activateAPIKeyService(apiClient, svc)
	}
}

// activateCredentialFreeService activates a service that needs no credentials (e.g. iMessage).
func activateCredentialFreeService(apiClient *client.Client, svc client.ServiceInfo) error {
	fmt.Printf("\n  Activating %s...\n", svc.Name)
	if err := apiClient.ActivateService(svc.ID); err != nil {
		return err
	}
	fmt.Printf("  %s %s connected.\n\n", green.Render("✓"), svc.Name)
	return nil
}

// activateAPIKeyService prompts for an API key/token and activates the service.
func activateAPIKeyService(apiClient *client.Client, svc client.ServiceInfo) error {
	var token string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Enter API key for %s", svc.Name)).
				EchoMode(huh.EchoModePassword).
				Value(&token),
		),
	).Run(); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}

	fmt.Printf("\n  Connecting %s...\n", svc.Name)
	if err := apiClient.ActivateWithKey(svc.ID, token, ""); err != nil {
		return err
	}
	fmt.Printf("  %s %s connected.\n\n", green.Render("✓"), svc.Name)
	return nil
}

// activateOAuthService handles OAuth activation. For Google services, if
// client_id/client_secret are missing from config, it collects them, patches
// config.yaml, and returns needsRestart=true so the server can be restarted
// with the updated adapter configuration.
func activateOAuthService(apiClient *client.Client, svc client.ServiceInfo, dataDir string) (needsRestart bool, err error) {
	// Google OAuth requires client_id/secret in config before the server starts
	// (adapters are immutable singletons). If they're absent, collect them and
	// request a config-reload restart.
	if strings.HasPrefix(svc.ID, "google.") {
		configPath := filepath.Join(dataDir, "config.yaml")
		hasCreds, err := googleCredsPresent(configPath)
		if err != nil {
			return false, fmt.Errorf("reading config: %w", err)
		}
		if !hasCreds {
			restart, err := collectAndPatchGoogleCreds(configPath, svc.Name)
			if err != nil {
				return false, err
			}
			if restart {
				return true, nil
			}
			// User left creds blank — skip.
			return false, nil
		}
	}

	// Prompt the user before opening the browser.
	proceed := true
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Open browser to authorize %s?", svc.Name)).
				Affirmative("Yes").
				Negative("Cancel").
				Value(&proceed),
		),
	).Run(); err != nil {
		return false, err
	}
	if !proceed {
		return false, nil
	}

	// Start a local callback listener so the OAuth HTML page can signal us.
	port, doneCh, cleanup := startOAuthListener()
	defer cleanup()

	cliCallback := fmt.Sprintf("http://127.0.0.1:%d/oauth-done", port)
	oauthResp, err := apiClient.GetOAuthURL(svc.ID, "", cliCallback)
	if err != nil {
		return false, fmt.Errorf("getting OAuth URL: %w", err)
	}
	if oauthResp.AlreadyAuthorized {
		fmt.Printf("  %s %s already authorized.\n\n", green.Render("✓"), svc.Name)
		return false, nil
	}

	fmt.Printf("\n  Opening browser for %s OAuth...\n", svc.Name)
	if !browser.Open(oauthResp.URL) {
		fmt.Println(dim.Padding(0, 2).Render("  Could not open browser. Visit the URL manually:"))
		fmt.Println(dim.Padding(0, 2).Render("  " + oauthResp.URL))
	}

	fmt.Println(dim.Padding(0, 2).Render("  Waiting for OAuth to complete..."))
	<-doneCh

	fmt.Printf("  %s %s connected.\n\n", green.Render("✓"), svc.Name)
	return false, nil
}

// collectAndPatchGoogleCreds prompts for Google OAuth client_id/secret,
// patches config.yaml, and returns true so the caller triggers a server restart.
func collectAndPatchGoogleCreds(configPath, serviceName string) (restart bool, err error) {
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render(fmt.Sprintf(
		"  %s requires Google OAuth credentials (client_id and client_secret).\n"+
			"  Create them at https://console.cloud.google.com → APIs & Services → Credentials.",
		serviceName,
	)))
	fmt.Println()

	var clientID, clientSecret string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Google OAuth Client ID").
				Value(&clientID),
			huh.NewInput().
				Title("Google OAuth Client Secret").
				EchoMode(huh.EchoModePassword).
				Value(&clientSecret),
		),
	).Run(); err != nil {
		return false, err
	}

	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return false, nil
	}

	if err := patchGoogleConfig(configPath, clientID, clientSecret); err != nil {
		return false, fmt.Errorf("patching config: %w", err)
	}
	return true, nil
}

// manageConnectedService shows options for an already-connected service:
// add another account, disconnect, or go back.
func manageConnectedService(apiClient *client.Client, svc client.ServiceInfo, dataDir string) error {
	var action string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("%s — already connected", svc.Name)).
				Options(
					huh.NewOption("Add another account", "add"),
					huh.NewOption("Disconnect", "disconnect"),
					huh.NewOption("← Back", "back"),
				).
				Value(&action),
		),
	).Run(); err != nil {
		return err
	}

	switch action {
	case "add":
		_, err := activateService(apiClient, svc, dataDir)
		return err
	case "disconnect":
		confirmed := false
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Disconnect %s?", svc.Name)).
					Affirmative("Yes, disconnect").
					Negative("Cancel").
					Value(&confirmed),
			),
		).Run(); err != nil {
			return err
		}
		if !confirmed {
			return nil
		}
		if err := apiClient.DeactivateService(svc.ID, svc.Alias); err != nil {
			return err
		}
		fmt.Printf("  %s disconnected.\n\n", svc.Name)
	}
	return nil
}

// googleCredsPresent reports whether services.google.client_id is set in the
// config file at configPath.
func googleCredsPresent(configPath string) (bool, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false, err
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false, err
	}
	services, ok := raw["services"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	google, ok := services["google"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	id, _ := google["client_id"].(string)
	return strings.TrimSpace(id) != "", nil
}

// patchGoogleConfig inserts Google OAuth credentials into config.yaml using
// line-level editing to avoid the corruption caused by YAML round-tripping
// through map[string]interface{} (which re-encodes all values and can mangle
// strings containing special characters).
func patchGoogleConfig(configPath, clientID, clientSecret string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	content := string(data)
	googleBlock := fmt.Sprintf("  google:\n    client_id: \"%s\"\n    client_secret: \"%s\"\n", clientID, clientSecret)

	// If there's already a google: section under services:, replace it.
	// Otherwise, append it after the services: line.
	if strings.Contains(content, "  google:") {
		// Replace existing google block (from "  google:" to next top-level or peer key).
		lines := strings.Split(content, "\n")
		var out []string
		inGoogle := false
		for _, line := range lines {
			if strings.HasPrefix(line, "  google:") {
				inGoogle = true
				out = append(out, strings.TrimRight(googleBlock, "\n"))
				continue
			}
			if inGoogle {
				// Still inside google block if indented deeper than 2 spaces.
				trimmed := strings.TrimLeft(line, " ")
				indent := len(line) - len(trimmed)
				if indent > 2 && trimmed != "" {
					continue // skip old google sub-keys
				}
				inGoogle = false
			}
			out = append(out, line)
		}
		content = strings.Join(out, "\n")
	} else if strings.Contains(content, "services:") {
		// Append google block after services: line.
		content = strings.Replace(content, "services:\n", "services:\n"+googleBlock, 1)
	} else {
		// No services section — append one.
		content += "\nservices:\n" + googleBlock
	}

	return os.WriteFile(configPath, []byte(content), 0600)
}

// injectMissingGoogleServices prepends known Google services to the list if
// the server didn't return them (because adapters aren't registered without
// OAuth creds in the config).
func injectMissingGoogleServices(services []client.ServiceInfo) []client.ServiceInfo {
	have := make(map[string]bool, len(services))
	for _, s := range services {
		have[s.ID] = true
	}
	var missing []client.ServiceInfo
	for _, gs := range knownGoogleServices {
		if !have[gs.ID] {
			missing = append(missing, gs)
		}
	}
	// Prepend so Google services appear first.
	return append(missing, services...)
}
