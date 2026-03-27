package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
)

// Services runs the interactive service connection menu. Requires a running
// daemon. If Google OAuth credentials are collected, the daemon is restarted
// automatically so the new adapters are loaded.
func Services() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	apiClient, err := NewAPIClient()
	if err != nil {
		return err
	}

	needsRestart, err := runServiceSetup(apiClient, dataDir)
	if err == huh.ErrUserAborted {
		return nil
	}
	if err != nil {
		return err
	}

	if needsRestart {
		fmt.Println()
		fmt.Println(dim.Padding(0, 2).Render("  Restarting daemon with updated configuration..."))
		if err := restartDaemon(); err != nil {
			return fmt.Errorf("restarting daemon: %w", err)
		}

		// Wait for the daemon to be fully healthy before reconnecting.
		serverURL, _, _ := readLocalSession(dataDir)
		if serverURL == "" {
			serverURL = "http://127.0.0.1:25297"
		}
		if err := waitForServer(serverURL); err != nil {
			return fmt.Errorf("daemon did not become healthy after restart: %w", err)
		}

		fmt.Println(green.Padding(0, 2).Render("  ✓ Daemon restarted"))
		fmt.Println()

		// Re-authenticate after restart and resume service setup.
		apiClient, err = NewAPIClient()
		if err != nil {
			return fmt.Errorf("reconnecting after restart: %w", err)
		}
		if _, err := runServiceSetup(apiClient, dataDir); err != nil && err != huh.ErrUserAborted {
			return err
		}
	}

	return nil
}

// ServicesList prints connected and available services.
func ServicesList(asJSON bool) error {
	apiClient, err := NewAPIClient()
	if err != nil {
		return err
	}

	resp, err := apiClient.GetServices()
	if err != nil {
		return fmt.Errorf("fetching services: %w", err)
	}

	services := injectMissingGoogleServices(resp.Services)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(services)
	}

	if len(services) == 0 {
		fmt.Println("No services available.")
		return nil
	}

	fmt.Printf("%-20s  %-12s  %s\n", "SERVICE", "STATUS", "DESCRIPTION")
	for _, s := range services {
		status := s.Status
		if status == "activated" {
			status = green.Render("connected")
		} else {
			status = dim.Render("available")
		}
		name := s.Name
		if s.Alias != "" {
			name += dim.Render(" (" + s.Alias + ")")
		}
		fmt.Printf("%-20s  %-12s  %s\n", name, status, s.Description)
	}
	return nil
}

// ServicesAdd connects a service. If serviceID is empty, an interactive picker
// is shown. Requires a running daemon.
func ServicesAdd(serviceID string) error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	apiClient, err := NewAPIClient()
	if err != nil {
		return err
	}

	resp, err := apiClient.GetServices()
	if err != nil {
		return fmt.Errorf("fetching services: %w", err)
	}

	services := injectMissingGoogleServices(resp.Services)

	if serviceID == "" {
		// Interactive picker — show only unconnected services.
		var opts []huh.Option[string]
		for _, s := range services {
			if s.Status != "activated" {
				opts = append(opts, huh.NewOption(serviceLabel(s), serviceKey(s)))
			}
		}
		if len(opts) == 0 {
			fmt.Println("All services are already connected.")
			return nil
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
			if err == huh.ErrUserAborted {
				return nil
			}
			return err
		}
		serviceID = choice
	}

	// Find the service.
	var found bool
	for _, s := range services {
		if serviceKey(s) == serviceID || s.ID == serviceID || strings.EqualFold(s.Name, serviceID) {
			needsRestart, err := activateService(apiClient, s, dataDir)
			if err != nil {
				return fmt.Errorf("connecting %s: %w", s.Name, err)
			}
			if needsRestart {
				fmt.Println(dim.Padding(0, 2).Render("  Restarting daemon with updated configuration..."))
				if err := restartDaemon(); err != nil {
					return fmt.Errorf("restarting daemon: %w", err)
				}

				serverURL, _, _ := readLocalSession(dataDir)
				if serverURL == "" {
					serverURL = "http://127.0.0.1:25297"
				}
				if err := waitForServer(serverURL); err != nil {
					return fmt.Errorf("daemon did not become healthy after restart: %w", err)
				}

				fmt.Println(green.Padding(0, 2).Render("  ✓ Daemon restarted"))
				fmt.Println()

				// Re-authenticate and activate the service now that creds are loaded.
				apiClient, err = NewAPIClient()
				if err != nil {
					return fmt.Errorf("reconnecting after restart: %w", err)
				}
				if _, activateErr := activateService(apiClient, s, dataDir); activateErr != nil {
					return fmt.Errorf("connecting %s after restart: %w", s.Name, activateErr)
				}
			}
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("service %q not found", serviceID)
	}
	return nil
}

// ServicesRemove disconnects a service. Requires a running daemon.
func ServicesRemove(serviceID string) error {
	apiClient, err := NewAPIClient()
	if err != nil {
		return err
	}

	resp, err := apiClient.GetServices()
	if err != nil {
		return fmt.Errorf("fetching services: %w", err)
	}

	// Find the service by ID, key, or name.
	for _, s := range resp.Services {
		if serviceKey(s) == serviceID || s.ID == serviceID || strings.EqualFold(s.Name, serviceID) {
			if s.Status != "activated" {
				return fmt.Errorf("%s is not connected", s.Name)
			}
			if err := apiClient.DeactivateService(s.ID, s.Alias); err != nil {
				return fmt.Errorf("disconnecting %s: %w", s.Name, err)
			}
			fmt.Printf("  %s disconnected.\n", s.Name)
			return nil
		}
	}

	return fmt.Errorf("service %q not found", serviceID)
}

// restartDaemon stops and starts the daemon service so it picks up config
// changes (e.g. new Google OAuth credentials).
func restartDaemon() error {
	if err := Stop(); err != nil {
		return err
	}
	return Start()
}
