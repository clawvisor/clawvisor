package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	qrterminal "github.com/mdp/qrterminal/v3"
	"gopkg.in/yaml.v3"
)

// Pair initiates a device pairing session: starts a pairing flow via the
// daemon API, displays a QR code and fallback code, then waits for a mobile
// device to complete pairing.
func Pair() error {
	dataDir, err := ensureDataDir()
	if err != nil {
		return err
	}

	// Authenticate with the running daemon.
	serverURL, magicToken, err := readLocalSession(dataDir)
	if err != nil {
		return fmt.Errorf("could not read local session — is the daemon running?\n  Start it with: clawvisor start")
	}

	apiClient, err := authenticateClient(serverURL, magicToken)
	if err != nil {
		return fmt.Errorf("could not authenticate with daemon: %w", err)
	}

	// Read relay config to get daemon_id and relay URL.
	daemonID, relayHost, err := readRelayConfig(dataDir)
	if err != nil {
		return fmt.Errorf("could not read relay config: %w", err)
	}
	if daemonID == "" {
		return fmt.Errorf("no daemon_id configured — relay registration may have failed during setup.\nRe-run `clawvisor setup` once the relay is reachable")
	}

	// Start pairing session.
	resp, err := apiClient.StartPairing()
	if err != nil {
		return fmt.Errorf("starting pairing session: %w", err)
	}

	// Construct the App Clip URL.
	clipURL := fmt.Sprintf("https://clawvisor.com/clip/pair?d=%s&t=%s&r=%s",
		daemonID, resp.PairingToken, relayHost)

	// Snapshot existing devices so we can detect the new one.
	existingDevices, _ := apiClient.ListDevices()
	existingIDs := make(map[string]bool, len(existingDevices))
	for _, d := range existingDevices {
		existingIDs[d.ID] = true
	}

	// Display QR code and pairing info.
	fmt.Println()
	fmt.Println(bold.Padding(0, 2).Render("Pair a mobile device"))
	fmt.Println(dim.Padding(0, 2).Render("Scan this QR code with your phone's camera:"))
	fmt.Println()

	qrterminal.GenerateWithConfig(clipURL, qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 2,
	})

	fmt.Println()
	fmt.Printf("  Pairing code: %s\n", bold.Render(formatCode(resp.Code)))
	fmt.Println(dim.Padding(0, 2).Render("Enter this code on your phone if prompted."))

	// Also fetch a relay pairing code for MCP/remote use.
	if pairingCode, err := apiClient.GetPairingCode(); err == nil {
		fmt.Println()
		fmt.Printf("  Daemon ID:    %s\n", bold.Render(pairingCode.DaemonID))
		fmt.Printf("  Pairing code: %s\n", bold.Render(formatCode(pairingCode.Code)))
		fmt.Printf("  Code expires in %d minutes.\n", pairingCode.ExpiresIn/60)
		fmt.Printf("  Enter these at: https://%s/authorize\n", relayHost)
	}

	fmt.Println()
	fmt.Println(dim.Padding(0, 2).Render("Waiting for pairing to complete..."))

	// Poll for a new device to appear (5 min timeout matching server-side expiry).
	deadline := time.Now().Add(5 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("pairing timed out — try again with: clawvisor pair")
			}
			devices, err := apiClient.ListDevices()
			if err != nil {
				continue
			}
			for _, d := range devices {
				if !existingIDs[d.ID] {
					fmt.Printf("\n  %s Device paired: %s\n\n", green.Render("✓"), d.DeviceName)
					return nil
				}
			}
		}
	}
}

// readRelayConfig reads daemon_id and relay URL from config.yaml.
func readRelayConfig(dataDir string) (daemonID, relayHost string, err error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return "", "", err
	}
	var cfg struct {
		Relay struct {
			DaemonID string `yaml:"daemon_id"`
			URL      string `yaml:"url"`
		} `yaml:"relay"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", "", err
	}

	// Extract host from relay URL (strip scheme).
	host := cfg.Relay.URL
	host = strings.TrimPrefix(host, "wss://")
	host = strings.TrimPrefix(host, "ws://")

	return cfg.Relay.DaemonID, host, nil
}

// formatCode inserts a dash in the middle of a 6-digit code for readability.
func formatCode(code string) string {
	if len(code) == 6 {
		return code[:3] + "-" + code[3:]
	}
	return code
}
