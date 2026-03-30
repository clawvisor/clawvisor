package daemon

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/clawvisor/clawvisor/internal/browser"
	"github.com/clawvisor/clawvisor/internal/tui/client"
)

const continueOption = "__continue__"

// runServiceSetup presents the interactive service-selection loop.
func runServiceSetup(apiClient *client.Client, dataDir string) error {
	if nonInteractive() {
		fmt.Println(dim.Padding(0, 2).Render("  Skipping interactive service setup (non-interactive mode)."))
		fmt.Println(dim.Padding(0, 2).Render("  Connect services later with: clawvisor setup"))
		return nil
	}

	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Connect services"))
	fmt.Println(dim.Padding(0, 2).Render("Connect the services you want Clawvisor to manage."))
	fmt.Println()

	for {
		resp, err := apiClient.GetServices()
		if err != nil {
			return fmt.Errorf("fetching services: %w", err)
		}

		services := resp.Services

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
				return nil
			}
			return nil
		}

		selected, err := presentServiceMenu(services)
		if err != nil {
			if err == huh.ErrUserAborted {
				return huh.ErrUserAborted
			}
			return err
		}

		if selected == continueOption {
			return nil
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

		if err := activateService(apiClient, svc, dataDir); err != nil {
			fmt.Printf("\n  %s\n\n", dim.Render("Could not connect: "+err.Error()))
			continue
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
// OAuth services first, then others, with "── Continue →" at the top.
func presentServiceMenu(services []client.ServiceInfo) (selected string, err error) {
	// Partition: OAuth services first, then the rest.
	var oauth, other []client.ServiceInfo
	for _, s := range services {
		if s.OAuth {
			oauth = append(oauth, s)
		} else {
			other = append(other, s)
		}
	}

	var opts []huh.Option[string]

	// Continue is prominent at the top.
	opts = append(opts, huh.NewOption(bold.Render("Done connecting services →"), continueOption))

	// OAuth services first.
	for _, list := range [][]client.ServiceInfo{oauth, other} {
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
func activateService(apiClient *client.Client, svc client.ServiceInfo, dataDir string) error {
	switch {
	case svc.CredentialFree:
		return activateCredentialFreeService(apiClient, svc)
	case svc.OAuth:
		return activateOAuthService(apiClient, svc, dataDir)
	default:
		return activateAPIKeyService(apiClient, svc)
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
	if svc.SetupURL != "" {
		fmt.Println(dim.Padding(0, 2).Render(fmt.Sprintf("  Create an API key at: %s", svc.SetupURL)))
	}

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

// activateOAuthService handles OAuth activation. For services using the
// Google OAuth endpoint, if app credentials aren't configured yet, it
// collects them and stores them in the system vault (no restart required).
func activateOAuthService(apiClient *client.Client, svc client.ServiceInfo, dataDir string) error {
	// Google OAuth requires app credentials (client_id/secret) in the system vault.
	// If they're absent, collect them via prompt and store via the API.
	if svc.OAuthEndpoint == "google" {
		configured, err := apiClient.GoogleOAuthConfigured()
		if err != nil {
			return fmt.Errorf("checking Google OAuth config: %w", err)
		}
		if !configured {
			if err := collectAndStoreGoogleCreds(apiClient, svc.Name); err != nil {
				return err
			}
			// Re-check — user may have left fields blank.
			configured, _ = apiClient.GoogleOAuthConfigured()
			if !configured {
				return nil // user skipped, back to menu
			}
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
		return err
	}
	if !proceed {
		return nil
	}

	// Start a local callback listener so the OAuth HTML page can signal us.
	port, doneCh, cleanup := startOAuthListener()
	defer cleanup()

	cliCallback := fmt.Sprintf("http://127.0.0.1:%d/oauth-done", port)
	oauthResp, err := apiClient.GetOAuthURL(svc.ID, "", cliCallback)
	if err != nil {
		return fmt.Errorf("getting OAuth URL: %w", err)
	}
	if oauthResp.AlreadyAuthorized {
		fmt.Printf("  %s %s already authorized.\n\n", green.Render("✓"), svc.Name)
		return nil
	}

	fmt.Printf("\n  Opening browser for %s OAuth...\n", svc.Name)
	if !browser.Open(oauthResp.URL) {
		fmt.Println(dim.Padding(0, 2).Render("  Could not open browser. Visit the URL manually:"))
		fmt.Println(dim.Padding(0, 2).Render("  " + oauthResp.URL))
	}

	fmt.Println(dim.Padding(0, 2).Render("  Waiting for OAuth to complete..."))
	<-doneCh

	fmt.Printf("  %s %s connected.\n\n", green.Render("✓"), svc.Name)
	return nil
}

// collectAndStoreGoogleCreds prompts for Google OAuth client_id/secret and
// stores them in the system vault via the API.
func collectAndStoreGoogleCreds(apiClient *client.Client, serviceName string) error {
	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render(fmt.Sprintf(
		"  %s requires Google OAuth credentials (client_id and client_secret).",
		serviceName,
	)))
	fmt.Println(dim.Padding(0, 2).Render(
		"  Follow the setup guide: https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md",
	))
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
		return err
	}

	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return nil
	}

	if err := apiClient.SetGoogleOAuthConfig(clientID, clientSecret); err != nil {
		return fmt.Errorf("storing Google OAuth credentials: %w", err)
	}

	fmt.Printf("  %s Google OAuth credentials saved.\n\n", green.Render("✓"))
	return nil
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
		return activateService(apiClient, svc, dataDir)
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
