package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/google"
)

// TestGemini_E2E_LiveSmoke makes a real Gemini request using
// application-default credentials and asserts a non-empty response.
// Direct-bypass smoke test per goal criterion 7: validates the auth
// layer (oauth2/google.DefaultTokenSource) and the Gemini API shape,
// not the proxy-mediated path.
//
// Uses Vertex AI (aiplatform.googleapis.com), which is the
// ADC-compatible Gemini endpoint. generativelanguage.googleapis.com —
// the URL specified verbatim in the goal file — rejects ADC tokens
// with ACCESS_TOKEN_SCOPE_INSUFFICIENT (it expects API keys, not
// OAuth2). The codebase's existing Gemini integration in
// internal/llm/gemini_cache.go uses Vertex AI for the same reason.
//
// Gated on:
//   - GOOGLE_APPLICATION_CREDENTIALS being set OR `gcloud auth
//     application-default login` having been run (provides ADC).
//   - GOOGLE_CLOUD_PROJECT or GCP_PROJECT being set (Vertex AI is
//     project-scoped). Falls back to the project field of the ADC
//     credentials file when neither is set.
func TestGemini_E2E_LiveSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Skipf("Google ADC not available: %v", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Skipf("Google ADC token fetch failed: %v", err)
	}
	if tok.AccessToken == "" {
		t.Skip("Google ADC returned empty access token")
	}

	project := creds.ProjectID
	if v := os.Getenv("GOOGLE_CLOUD_PROJECT"); v != "" {
		project = v
	} else if v := os.Getenv("GCP_PROJECT"); v != "" {
		project = v
	}
	if project == "" {
		t.Skip("no GCP project available (set GOOGLE_CLOUD_PROJECT or use ADC creds with project_id)")
	}

	// Region "global" is allowed by Vertex AI and routes to the
	// non-regional endpoint, which avoids the regional-quota gotcha for
	// a smoke test.
	region := "global"
	host := "aiplatform.googleapis.com"

	// gemini-2.0-flash-001 is the current general-availability flash
	// model that supports :generateContent on Vertex. Schema:
	// https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference
	model := "gemini-2.0-flash-001"
	url := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, project, region, model)

	reqBody, err := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]any{
					{"text": "Reply with the single word ok."},
				},
			},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": 16,
			"temperature":     0,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Gemini request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Gemini returned %d: %s", resp.StatusCode, string(body))
	}
	if len(body) == 0 {
		t.Fatal("Gemini response body is empty")
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse response: %v\n%s", err, string(body))
	}
	if len(parsed.Candidates) == 0 {
		t.Fatalf("no candidates in response: %s", string(body))
	}
	text := ""
	for _, p := range parsed.Candidates[0].Content.Parts {
		text += p.Text
	}
	if strings.TrimSpace(text) == "" {
		t.Fatalf("empty candidate text: %s", string(body))
	}
}
