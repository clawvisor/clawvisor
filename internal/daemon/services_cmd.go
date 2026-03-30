package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
)

// Services runs the interactive service connection menu. Requires a running daemon.
func Services() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	apiClient, err := NewAPIClient()
	if err != nil {
		return err
	}

	err = runServiceSetup(apiClient, dataDir)
	if err == huh.ErrUserAborted {
		return nil
	}
	return err
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

	services := resp.Services

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

	services := resp.Services

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
	for _, s := range services {
		if serviceKey(s) == serviceID || s.ID == serviceID || strings.EqualFold(s.Name, serviceID) {
			if err := activateService(apiClient, s, dataDir); err != nil {
				return fmt.Errorf("connecting %s: %w", s.Name, err)
			}
			return nil
		}
	}

	return fmt.Errorf("service %q not found", serviceID)
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
