package version

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is set at build time via -ldflags.
// Example: go build -ldflags="-X github.com/clawvisor/clawvisor/pkg/version.Version=0.3.0"
var Version = "dev"

// Environment is set at build time via -ldflags. Valid values: "production" (default), "staging".
// Example: go build -ldflags="-X github.com/clawvisor/clawvisor/pkg/version.Environment=staging"
var Environment = "production"

// IsStaging returns true when the binary was built for the staging environment.
func IsStaging() bool {
	return Environment == "staging"
}

// RelayURL returns the default relay WebSocket URL for the current environment.
func RelayURL() string {
	if IsStaging() {
		return "wss://relay.staging.clawvisor.com"
	}
	return "wss://relay.clawvisor.com"
}

// PushURL returns the default push service URL for the current environment.
func PushURL() string {
	if IsStaging() {
		return "https://push.staging.clawvisor.com"
	}
	return "https://push.clawvisor.com"
}

// HaikuProxyURL returns the haiku proxy base URL for the current environment.
// Includes /v1 so the LLM client's "/messages" append hits /v1/messages.
func HaikuProxyURL() string {
	if IsStaging() {
		return "https://hp.staging.clawvisor.com/v1"
	}
	return "https://hp.clawvisor.com/v1"
}

// CORSOrigins returns the allowed CORS origins for the current environment.
func CORSOrigins() []string {
	if IsStaging() {
		return []string{"https://relay.staging.clawvisor.com", "https://app.staging.clawvisor.com"}
	}
	return []string{"https://relay.clawvisor.com", "https://app.clawvisor.com"}
}

const (
	githubOwner = "clawvisor"
	githubRepo  = "clawvisor"
	cacheTTL    = 1 * time.Hour
)

// Info holds current and latest version info.
type Info struct {
	Current       string `json:"current"`
	Latest        string `json:"latest,omitempty"`
	UpdateAvail   bool   `json:"update_available"`
	ReleaseURL    string `json:"release_url,omitempty"`
	UpgradeCmd    string `json:"upgrade_command,omitempty"`
}

var (
	cacheMu     sync.Mutex
	cachedInfo  *Info
	cachedAt    time.Time
)

// Check returns version info, fetching latest from GitHub if cache is stale.
func Check() *Info {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	if cachedInfo != nil && time.Since(cachedAt) < cacheTTL {
		return cachedInfo
	}

	info := &Info{
		Current:    Version,
		UpgradeCmd: "go install github.com/clawvisor/clawvisor/cmd/clawvisor@latest",
	}

	latest, url, err := fetchLatestRelease()
	if err == nil && latest != "" {
		info.Latest = latest
		info.ReleaseURL = url
		info.UpdateAvail = isNewer(latest, Version)
	}

	cachedInfo = info
	cachedAt = time.Now()
	return info
}

// GetCurrent returns the current version without checking for updates.
func GetCurrent() string {
	return Version
}

// fetchLatestRelease queries the GitHub API for the latest release tag.
func fetchLatestRelease() (version, url string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}

	return strings.TrimPrefix(release.TagName, "v"), release.HTMLURL, nil
}

// isNewer returns true if latest is a higher semver than current.
func isNewer(latest, current string) bool {
	if current == "dev" || current == "" {
		return false // dev builds don't show update prompts
	}
	lMajor, lMinor, lPatch := parseSemver(latest)
	cMajor, cMinor, cPatch := parseSemver(current)

	if lMajor != cMajor {
		return lMajor > cMajor
	}
	if lMinor != cMinor {
		return lMinor > cMinor
	}
	return lPatch > cPatch
}

func parseSemver(v string) (major, minor, patch int) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, "-", 2) // strip pre-release
	segments := strings.Split(parts[0], ".")
	if len(segments) >= 1 {
		major, _ = strconv.Atoi(segments[0])
	}
	if len(segments) >= 2 {
		minor, _ = strconv.Atoi(segments[1])
	}
	if len(segments) >= 3 {
		patch, _ = strconv.Atoi(segments[2])
	}
	return
}
