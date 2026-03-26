package haikuproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/pkg/version"
)

// Registration holds the response from the haiku proxy registration endpoint.
type Registration struct {
	Key      string  `json:"key"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	SpendCap float64 `json:"spend_cap"`
}

// Register creates a new haiku proxy key by calling POST /v1/register.
// The returned key can be used as ANTHROPIC_API_KEY with the proxy's base URL.
func Register(name string) (*Registration, error) {
	baseURL := version.HaikuProxyURL()

	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("connecting to haiku proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited — too many registrations from this IP today (max 10)")
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("haiku proxy returned HTTP %d", resp.StatusCode)
	}

	var reg Registration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &reg, nil
}

// Usage holds the spend information for a haiku proxy key.
type Usage struct {
	SpendCap   float64 `json:"spend_cap"`
	TotalSpent float64 `json:"total_spent"`
	Remaining  float64 `json:"remaining"`
}

// GetUsage fetches the current spend for a haiku proxy key.
func GetUsage(apiKey string) (*Usage, error) {
	baseURL := version.HaikuProxyURL()

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to haiku proxy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("haiku proxy usage returned HTTP %d", resp.StatusCode)
	}

	var usage Usage
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("decoding usage response: %w", err)
	}
	return &usage, nil
}

// BaseURL returns the haiku proxy base URL for the current environment.
func BaseURL() string {
	return version.HaikuProxyURL()
}
