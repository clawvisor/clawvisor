package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/clawvisor/clawvisor/internal/tui/client"
)

// cliSession is the JSON structure cached to ~/.clawvisor/.cli-session.
type cliSession struct {
	ServerURL   string `json:"server_url"`
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"` // Unix seconds
}

// NewAPIClient returns an authenticated TUI client connected to the running
// daemon. It caches the JWT in ~/.clawvisor/.cli-session and reuses it until
// it is close to expiring (within 60 seconds).
//
// The flow is:
//  1. Read cached session — if the JWT is still valid, reuse it.
//  2. Otherwise: POST /api/auth/magic/local (localhost-only, no auth) to get
//     a magic token, then POST /api/auth/magic to exchange it for a JWT.
//  3. Cache the new JWT for future calls.
func NewAPIClient() (*client.Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".clawvisor")
	sessionPath := filepath.Join(dataDir, ".cli-session")

	// Try cached session.
	if sess, err := readCLISession(sessionPath); err == nil {
		if time.Now().Unix() < sess.ExpiresAt-60 {
			cl := client.New(sess.ServerURL, "")
			cl.SetAccessToken(sess.AccessToken)
			return cl, nil
		}
	}

	// Resolve the server URL from the local session file.
	serverURL, _, _ := readLocalSession(dataDir)
	if serverURL == "" {
		serverURL = "http://127.0.0.1:25297"
	}

	// Get a fresh magic token from the localhost-only endpoint.
	magicToken, err := requestMagicToken(serverURL)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable — is it running? (%w)", err)
	}

	// Exchange it for a JWT.
	cl := client.New(serverURL, "")
	authResp, err := cl.LoginMagic(magicToken)
	if err != nil {
		return nil, fmt.Errorf("authenticating with daemon: %w", err)
	}

	// Cache the session.
	// The API returns expires_in but it's not in AuthResponse — use a
	// conservative default of 14 minutes (access tokens default to 15m).
	expiresAt := time.Now().Add(14 * time.Minute).Unix()
	_ = writeCLISession(sessionPath, &cliSession{
		ServerURL:   serverURL,
		AccessToken: authResp.AccessToken,
		ExpiresAt:   expiresAt,
	})

	return cl, nil
}

func readCLISession(path string) (*cliSession, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess cliSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	if sess.ServerURL == "" || sess.AccessToken == "" {
		return nil, fmt.Errorf("incomplete session")
	}
	return &sess, nil
}

func writeCLISession(path string, sess *cliSession) error {
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
