// Package drive implements the Clawvisor adapter for Google Drive.
package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

const serviceID = "google.drive"

// driveScopes are the OAuth scopes required by the Drive adapter.
var driveScopes = []string{
	"https://www.googleapis.com/auth/drive.readonly",
	"https://www.googleapis.com/auth/drive.file",
	"https://www.googleapis.com/auth/userinfo.email",
}

// DriveAdapter implements adapters.Adapter for Google Drive.
type DriveAdapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

func New(provider adapters.OAuthCredentialProvider) *DriveAdapter {
	return &DriveAdapter{oauthProvider: provider}
}

func (a *DriveAdapter) ServiceID() string { return serviceID }

func (a *DriveAdapter) SupportedActions() []string {
	return []string{"list_files", "get_file", "export_file", "create_file", "update_file", "search_files"}
}

func (a *DriveAdapter) RequiredScopes() []string { return driveScopes }

func (a *DriveAdapter) OAuthConfig() *oauth2.Config {
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       driveScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *DriveAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, driveScopes)
}

func (a *DriveAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

// FetchIdentity returns the Google account email for auto-alias detection.
func (a *DriveAdapter) FetchIdentity(ctx context.Context, credBytes []byte, _ map[string]string) (string, error) {
	client, err := a.httpClient(ctx, credBytes)
	if err != nil {
		return "", err
	}
	return credential.FetchGoogleEmail(ctx, client)
}

func (a *DriveAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential)
	if err != nil {
		return nil, err
	}
	switch req.Action {
	case "list_files":
		return a.listFiles(ctx, client, req.Params)
	case "get_file":
		return a.getFile(ctx, client, req.Params)
	case "export_file":
		return a.exportFile(ctx, client, req.Params)
	case "create_file":
		return a.createFile(ctx, client, req.Params)
	case "update_file":
		return a.updateFile(ctx, client, req.Params)
	case "search_files":
		return a.searchFiles(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("drive: unsupported action %q", req.Action)
	}
}

func (a *DriveAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("drive: %w", err)
	}
	ts := a.OAuthConfig().TokenSource(ctx, cred.ToOAuth2Token())
	return oauth2.NewClient(ctx, ts), nil
}

// ── list_files ────────────────────────────────────────────────────────────────

type fileItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MimeType    string `json:"mime_type"`
	ModifiedAt  string `json:"modified_at"`
	Size        string `json:"size,omitempty"`
	WebViewLink string `json:"web_view_link,omitempty"`
}

func (a *DriveAdapter) listFiles(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("pageSize", fmt.Sprintf("%d", maxResults))
	q.Set("fields", "files(id,name,mimeType,modifiedTime,size,webViewLink)")
	if query != "" {
		q.Set("q", query)
	}

	apiURL := "https://www.googleapis.com/drive/v3/files?" + q.Encode()
	var resp struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			ModifiedTime string `json:"modifiedTime"`
			Size         string `json:"size"`
			WebViewLink  string `json:"webViewLink"`
		} `json:"files"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("drive list_files: %w", err)
	}

	items := make([]fileItem, 0, len(resp.Files))
	for _, f := range resp.Files {
		items = append(items, fileItem{
			ID:          f.ID,
			Name:        format.SanitizeText(f.Name, format.MaxFieldLen),
			MimeType:    f.MimeType,
			ModifiedAt:  f.ModifiedTime,
			Size:        f.Size,
			WebViewLink: f.WebViewLink,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d file(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_file ──────────────────────────────────────────────────────────────────

// textMimeTypes are the MIME types for which content preview is returned.
var textMimeTypes = map[string]bool{
	"text/plain":        true,
	"text/markdown":     true,
	"application/json":  true,
	"text/csv":          true,
	"text/html":         true,
}

func (a *DriveAdapter) getFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	fileID, _ := params["file_id"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("drive get_file: file_id is required")
	}

	// Fetch metadata first.
	metaURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?fields=id,name,mimeType,modifiedTime,size,webViewLink",
		url.PathEscape(fileID))
	var meta struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		MimeType     string `json:"mimeType"`
		ModifiedTime string `json:"modifiedTime"`
		Size         string `json:"size"`
		WebViewLink  string `json:"webViewLink"`
	}
	if err := apiGET(ctx, client, metaURL, &meta); err != nil {
		return nil, fmt.Errorf("drive get_file: %w", err)
	}

	result := map[string]any{
		"id":           meta.ID,
		"name":         format.SanitizeText(meta.Name, format.MaxFieldLen),
		"mime_type":    meta.MimeType,
		"modified_at":  meta.ModifiedTime,
		"size":         meta.Size,
		"web_view_link": meta.WebViewLink,
	}

	// Fetch content preview for text types only.
	if textMimeTypes[meta.MimeType] {
		contentURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media",
			url.PathEscape(fileID))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					limited := io.LimitReader(resp.Body, int64(format.MaxBodyLen))
					contentBytes, _ := io.ReadAll(limited)
					result["content"] = format.SanitizeText(string(contentBytes), format.MaxBodyLen)
				}
			}
		}
	}

	return &adapters.Result{
		Summary: format.Summary("File: %s (%s)", meta.Name, meta.MimeType),
		Data:    result,
	}, nil
}

// ── export_file ───────────────────────────────────────────────────────────────

// exportFile exports a Google Workspace file (Docs, Sheets, Slides) to the
// requested MIME type using the Drive files.export endpoint.
func (a *DriveAdapter) exportFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	fileID, _ := params["file_id"].(string)
	targetMime, _ := params["mime_type"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("drive export_file: file_id is required")
	}
	if targetMime == "" {
		return nil, fmt.Errorf("drive export_file: mime_type is required")
	}

	// Fetch metadata so we can include the file name in the result.
	metaURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?fields=id,name,mimeType",
		url.PathEscape(fileID))
	var meta struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
	}
	if err := apiGET(ctx, client, metaURL, &meta); err != nil {
		return nil, fmt.Errorf("drive export_file: %w", err)
	}

	exportURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s/export?mimeType=%s",
		url.PathEscape(fileID), url.QueryEscape(targetMime))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exportURL, nil)
	if err != nil {
		return nil, fmt.Errorf("drive export_file: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("drive export_file: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(format.MaxBodyLen)))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("drive export_file: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	result := map[string]any{
		"id":              meta.ID,
		"name":            format.SanitizeText(meta.Name, format.MaxFieldLen),
		"source_mime_type": meta.MimeType,
		"export_mime_type": targetMime,
		"content":         format.SanitizeText(string(body), format.MaxBodyLen),
	}
	return &adapters.Result{
		Summary: format.Summary("Exported %s as %s", meta.Name, targetMime),
		Data:    result,
	}, nil
}

// ── create_file ───────────────────────────────────────────────────────────────

func (a *DriveAdapter) createFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	name, _ := params["name"].(string)
	content, _ := params["content"].(string)
	mimeType, _ := params["mime_type"].(string)
	parentFolderID, _ := params["parent_folder_id"].(string)

	if name == "" {
		return nil, fmt.Errorf("drive create_file: name is required")
	}
	if mimeType == "" {
		mimeType = "text/plain"
	}

	// Use multipart upload to set both metadata and content.
	meta := map[string]any{"name": name}
	if parentFolderID != "" {
		meta["parents"] = []string{parentFolderID}
	}

	metaBytes, _ := json.Marshal(meta)
	contentBytes := []byte(content)

	// Multipart body
	boundary := "clawvisor-drive-upload"
	var buf bytes.Buffer
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: application/json; charset=UTF-8\r\n\r\n")
	buf.Write(metaBytes)
	buf.WriteString("\r\n--" + boundary + "\r\n")
	buf.WriteString("Content-Type: " + mimeType + "\r\n\r\n")
	buf.Write(contentBytes)
	buf.WriteString("\r\n--" + boundary + "--")

	apiURL := "https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("drive create_file: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("drive create_file: parsing response: %w", err)
	}
	return &adapters.Result{
		Summary: format.Summary("Created file: %s", created.Name),
		Data:    map[string]string{"file_id": created.ID, "name": created.Name},
	}, nil
}

// ── update_file ───────────────────────────────────────────────────────────────

func (a *DriveAdapter) updateFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	fileID, _ := params["file_id"].(string)
	content, _ := params["content"].(string)
	if fileID == "" {
		return nil, fmt.Errorf("drive update_file: file_id is required")
	}

	// Get file metadata to determine MIME type.
	metaURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?fields=id,name,mimeType",
		url.PathEscape(fileID))
	var meta struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
	}
	if err := apiGET(ctx, client, metaURL, &meta); err != nil {
		return nil, fmt.Errorf("drive update_file: %w", err)
	}

	// Simple media upload to update content.
	uploadURL := fmt.Sprintf("https://www.googleapis.com/upload/drive/v3/files/%s?uploadType=media",
		url.PathEscape(fileID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, uploadURL, strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", meta.MimeType)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("drive update_file: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return &adapters.Result{
		Summary: format.Summary("Updated file: %s", meta.Name),
		Data:    map[string]string{"file_id": fileID, "name": meta.Name},
	}, nil
}

// ── search_files ──────────────────────────────────────────────────────────────

func (a *DriveAdapter) searchFiles(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("drive search_files: query is required")
	}
	// Delegate to list_files with the query set.
	params["query"] = fmt.Sprintf("fullText contains '%s'", strings.ReplaceAll(query, "'", "\\'"))
	return a.listFiles(ctx, client, params)
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func apiGET(ctx context.Context, client *http.Client, apiURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func paramInt(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
