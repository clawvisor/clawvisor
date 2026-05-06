package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ExtractToken extracts the bearer token from the JSON credential bytes.
// Expects {"token": "..."} or {"access_token": "..."}
func ExtractToken(credBytes []byte) (string, error) {
	var cred struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("parsing credential: %w", err)
	}
	token := cred.Token
	if token == "" {
		token = cred.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("credential missing token")
	}
	return token, nil
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// GraphGET makes a GET request to the Graph API and unmarshals the response.
func GraphGET(ctx context.Context, token, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("graph API GET %s: %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// GraphPOST makes a POST request to the Graph API and unmarshals the response.
func GraphPOST(ctx context.Context, token, url string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("graph API POST %s: %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}
