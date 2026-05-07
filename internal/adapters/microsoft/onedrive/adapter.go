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
			endpoint = fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/root:/%s:/children", url.PathEscape(folderPath))
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

	// First get metadata to know the name and size
	metaEndpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/items/%s", itemID)
	var meta struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := microsoft.GraphGET(ctx, client, metaEndpoint, &meta); err != nil {
		return nil, fmt.Errorf("onedrive download_file: metadata: %w", err)
	}

	downloadEndpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/items/%s/content", itemID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadEndpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download_file: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(format.MaxBodyLen)))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("onedrive download_file: status %d", resp.StatusCode)
	}

	contentType := mime.TypeByExtension(filepath.Ext(meta.Name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	result := map[string]any{
		"name":         format.SanitizeText(meta.Name, format.MaxFieldLen),
		"id":           itemID,
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

func (a *Adapter) uploadFile(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	path, _ := params["path"].(string)
	content, _ := params["content"].(string)

	if path == "" {
		return nil, fmt.Errorf("onedrive upload_file: path is required")
	}
	if content == "" {
		return nil, fmt.Errorf("onedrive upload_file: content is required")
	}

	path = strings.TrimPrefix(path, "/")
	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/drive/root:/%s:/content", url.PathEscape(path))

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
			"path": path,
		},
	}, nil
}

func isTextContent(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml"
}
