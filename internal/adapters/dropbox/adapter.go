// Package dropbox implements the Clawvisor adapter for Dropbox file
// upload and download operations that require the content endpoint.
package dropbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const contentURL = "https://content.dropboxapi.com/2"

// Adapter handles Dropbox actions that require the content endpoint
// (download and upload), which use Dropbox-API-Arg headers instead
// of JSON bodies.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

// Execute dispatches to the appropriate action handler.
func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, fmt.Errorf("dropbox: %w", err)
	}
	switch req.Action {
	case "download_file":
		return a.downloadFile(ctx, token, req.Params)
	case "upload_file":
		return a.uploadFile(ctx, token, req.Params)
	default:
		return nil, fmt.Errorf("dropbox: unsupported action %q", req.Action)
	}
}

// ── download_file ────────────────────────────────────────────────────────────

func (a *Adapter) downloadFile(ctx context.Context, token string, params map[string]any) (*adapters.Result, error) {
	path, _ := params["path"].(string)
	if path == "" {
		return nil, fmt.Errorf("dropbox download_file: path is required")
	}

	apiArg, _ := json.Marshal(map[string]string{"path": path})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, contentURL+"/files/download", nil)
	if err != nil {
		return nil, fmt.Errorf("dropbox download_file: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Dropbox-API-Arg", string(apiArg))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dropbox download_file: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(format.MaxBodyLen)))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("dropbox download_file: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	// Dropbox returns file metadata in the Dropbox-API-Result header.
	var meta struct {
		Name string `json:"name"`
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	if resultHeader := resp.Header.Get("Dropbox-API-Result"); resultHeader != "" {
		_ = json.Unmarshal([]byte(resultHeader), &meta)
	}

	// Determine if content is text or binary.
	contentType := resp.Header.Get("Content-Type")
	result := map[string]any{
		"name":         format.SanitizeText(meta.Name, format.MaxFieldLen),
		"id":           meta.ID,
		"size":         meta.Size,
		"content_type": contentType,
	}

	if isTextContent(contentType) {
		result["content"] = format.SanitizeText(string(body), format.MaxBodyLen)
	} else {
		result["encoding"] = "base64"
		result["content"] = base64.StdEncoding.EncodeToString(body)
	}

	return &adapters.Result{
		Summary: format.Summary("Downloaded %s (%d bytes)", meta.Name, meta.Size),
		Data:    result,
	}, nil
}

// ── upload_file ──────────────────────────────────────────────────────────────

func (a *Adapter) uploadFile(ctx context.Context, token string, params map[string]any) (*adapters.Result, error) {
	path, _ := params["path"].(string)
	content, _ := params["content"].(string)
	if path == "" {
		return nil, fmt.Errorf("dropbox upload_file: path is required")
	}
	if content == "" {
		return nil, fmt.Errorf("dropbox upload_file: content is required")
	}

	mode := "add"
	if m, ok := params["mode"].(string); ok && m != "" {
		mode = m
	}

	apiArg, _ := json.Marshal(map[string]any{
		"path":       path,
		"mode":       mode,
		"autorename": false,
		"mute":       false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, contentURL+"/files/upload", strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("dropbox upload_file: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Dropbox-API-Arg", string(apiArg))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dropbox upload_file: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("dropbox upload_file: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var uploaded struct {
		Name         string `json:"name"`
		ID           string `json:"id"`
		PathDisplay  string `json:"path_display"`
		Size         int64  `json:"size"`
		ContentHash  string `json:"content_hash"`
	}
	if err := json.Unmarshal(body, &uploaded); err != nil {
		return nil, fmt.Errorf("dropbox upload_file: parsing response: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Uploaded %s (%d bytes)", uploaded.Name, uploaded.Size),
		Data: map[string]any{
			"name":         format.SanitizeText(uploaded.Name, format.MaxFieldLen),
			"id":           uploaded.ID,
			"path":         uploaded.PathDisplay,
			"size":         uploaded.Size,
			"content_hash": uploaded.ContentHash,
		},
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func extractToken(credBytes []byte) (string, error) {
	var cred struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("credential missing token")
	}
	return cred.Token, nil
}

func isTextContent(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml"
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
