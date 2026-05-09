package onedrive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/microsoft"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Adapter handles Go override actions for Microsoft OneDrive.
type Adapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

// New creates a OneDrive adapter with the given OAuth credential provider
// for automatic token refresh.
func New(provider adapters.OAuthCredentialProvider) *Adapter {
	return &Adapter{oauthProvider: provider}
}

func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := microsoft.HTTPClient(ctx, req.Credential, a.oauthProvider)
	if err != nil {
		return nil, fmt.Errorf("onedrive: %w", err)
	}

	switch req.Action {
	case "list_files":
		return a.listFiles(ctx, client, req.Params)
	case "download_file":
		return a.downloadFile(ctx, client, req.Params)
	case "upload_file":
		return a.uploadFile(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("onedrive: unsupported action %q", req.Action)
	}
}

func (a *Adapter) listFiles(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	folderPath, _ := params["folder_path"].(string)
	top := 25
	if t, ok := params["top"].(float64); ok {
		top = int(t)
	} else if t, ok := params["top"].(int); ok {
		top = t
	}
	selectFields, _ := params["select"].(string)
	if selectFields == "" {
		selectFields = "id,name,size,lastModifiedDateTime,folder,file"
	}

	endpoint := "https://graph.microsoft.com/v1.0/me/drive/root/children"
	if folderPath != "" {
		// Clean up path
		folderPath = strings.TrimPrefix(folderPath, "/")
		if folderPath != "" {
			endpoint = fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/root:/%s:/children", escapePath(folderPath))
		}
	}

	endpoint = fmt.Sprintf("%s?$top=%d&$select=%s", endpoint, top, selectFields)

	var out struct {
		Value []map[string]any `json:"value"`
	}

	if err := microsoft.GraphGET(ctx, client, endpoint, &out); err != nil {
		return nil, fmt.Errorf("onedrive list_files: %w", err)
	}

	// Format response to match YAML definition expectation
	var items []map[string]any
	for _, item := range out.Value {
		formatted := map[string]any{
			"id":   item["id"],
			"name": item["name"],
			"size": item["size"],
		}
		if lm, ok := item["lastModifiedDateTime"]; ok {
			formatted["modified"] = lm
		}
		if _, ok := item["folder"]; ok {
			formatted["type"] = "folder"
		} else {
			formatted["type"] = "file"
		}
		items = append(items, formatted)
	}

	return &adapters.Result{
		Summary: format.Summary("%d file(s)", len(items)),
		Data:    items,
	}, nil
}

func (a *Adapter) downloadFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	itemID, _ := params["item_id"].(string)
	if itemID == "" {
		return nil, fmt.Errorf("onedrive download_file: item_id is required")
	}

	// Determine the correct endpoint. If itemID looks like a path (e.g., starts with "root:"),
	// use the root path syntax. Otherwise, assume it's a DriveItem ID.
	var metaEndpoint string
	if strings.HasPrefix(itemID, "root:") || strings.Contains(itemID, "/") {
		// If it's a path, it might be /Documents/file.txt or root:/Documents/file.txt
		path := strings.TrimPrefix(itemID, "/")
		if !strings.HasPrefix(path, "root:") {
			path = "root:/" + path
		}
		// Handle the trailing colon if it's a path
		if strings.HasPrefix(path, "root:/") && !strings.HasSuffix(path, ":") {
			path += ":"
		}
		metaEndpoint = fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/%s", escapePath(path))
	} else {
		metaEndpoint = fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/items/%s", url.PathEscape(itemID))
	}

	// Use map[string]any to reliably capture the @ annotation key.
	var meta map[string]any
	if err := microsoft.GraphGET(ctx, client, metaEndpoint, &meta); err != nil {
		return nil, fmt.Errorf("onedrive download_file metadata: %w", err)
	}

	fileName, _ := meta["name"].(string)
	fileSize, _ := meta["size"].(float64) // JSON numbers unmarshal as float64

	downloadURL, _ := meta["@microsoft.graph.downloadUrl"].(string)

	if downloadURL == "" {
		// Fallback: use the /content endpoint
		contentEndpoint := metaEndpoint + "/content"
		return a.downloadViaContent(ctx, client, contentEndpoint, itemID, fileName, int64(fileSize))
	}


	// Use a plain HTTP client for the pre-signed URL download.
	// The download URL is pre-authenticated; sending an OAuth Bearer token
	// to the SharePoint download host causes a 401.
	plainClient := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := plainClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download_file: download error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("onedrive download_file: download status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(format.MaxBodyLen)))

	return a.buildDownloadResult(itemID, fileName, int64(fileSize), body), nil
}

// downloadViaContent handles the /content redirect flow by stripping the
// Authorization header on cross-host redirects.
func (a *Adapter) downloadViaContent(ctx context.Context, oauthClient *http.Client, endpoint, itemID, fileName string, fileSize int64) (*adapters.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	// Build a client that uses the OAuth transport but strips Authorization
	// on cross-host redirects (SharePoint download host rejects the token).
	dlClient := *oauthClient
	dlClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}

	resp, err := dlClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download_file: content redirect error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("onedrive download_file: download status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(format.MaxBodyLen)))

	return a.buildDownloadResult(itemID, fileName, fileSize, body), nil
}

// buildDownloadResult constructs the download response with appropriate
// content encoding (text or base64).
func (a *Adapter) buildDownloadResult(itemID, fileName string, fileSize int64, body []byte) *adapters.Result {
	contentType := mime.TypeByExtension(filepath.Ext(fileName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	result := map[string]any{
		"name":         format.SanitizeText(fileName, format.MaxFieldLen),
		"id":           itemID,
		"size":         fileSize,
		"content_type": contentType,
	}

	if isTextContent(contentType) {
		result["content"] = format.SanitizeText(string(body), format.MaxBodyLen)
	} else {
		result["encoding"] = "base64"
		result["content"] = base64.StdEncoding.EncodeToString(body)
	}

	return &adapters.Result{
		Summary: format.Summary("Downloaded %s (%d bytes)", fileName, fileSize),
		Data:    result,
	}
}

func (a *Adapter) uploadFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	path, _ := params["path"].(string)
	contentRaw, ok := params["content"]
	if !ok {
		return nil, fmt.Errorf("onedrive upload_file: content is required")
	}
	content, ok := contentRaw.(string)
	if !ok {
		return nil, fmt.Errorf("onedrive upload_file: content must be a string")
	}

	if path == "" {
		return nil, fmt.Errorf("onedrive upload_file: path is required")
	}

	// Microsoft Graph simple upload limit is 4MB.
	if len(content) > 4*1024*1024 {
		return nil, fmt.Errorf("onedrive upload_file: file too large for simple upload (max 4MB); resumable uploads not yet implemented")
	}

	path = strings.TrimPrefix(path, "/")
	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/root:/%s:/content", escapePath(path))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive upload_file: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("onedrive upload_file: status %d", resp.StatusCode)
	}

	var uploaded struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	// OneDrive returns the item metadata
	_ = json.Unmarshal(body, &uploaded)

	return &adapters.Result{
		Summary: format.Summary("Uploaded %s (%d bytes)", uploaded.Name, uploaded.Size),
		Data: map[string]any{
			"id":   uploaded.ID,
			"name": uploaded.Name,
			"size": uploaded.Size,
		},
	}, nil
}

func isTextContent(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml"
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
