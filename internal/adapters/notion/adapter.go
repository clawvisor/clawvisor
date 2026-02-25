// Package notion implements the Clawvisor adapter for Notion.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
)

const serviceID = "notion"

// storedCredential holds a Notion internal integration token.
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // e.g. "secret_..."
}

// NotionAdapter implements adapters.Adapter for Notion.
type NotionAdapter struct{}

func New() *NotionAdapter { return &NotionAdapter{} }

func (a *NotionAdapter) ServiceID() string { return serviceID }

func (a *NotionAdapter) SupportedActions() []string {
	return []string{
		"search", "get_page", "create_page", "update_page",
		"query_database", "list_databases",
	}
}

func (a *NotionAdapter) OAuthConfig() *oauth2.Config                         { return nil }
func (a *NotionAdapter) RequiredScopes() []string                            { return nil }
func (a *NotionAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("notion: OAuth token exchange not supported — use API key activation")
}

func (a *NotionAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("notion: invalid credential: %w", err)
	}
	if cred.Token == "" {
		return fmt.Errorf("notion: credential missing token")
	}
	return nil
}

func (a *NotionAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, err
	}
	client := notionClient(token)

	switch req.Action {
	case "search":
		return a.search(ctx, client, req.Params)
	case "get_page":
		return a.getPage(ctx, client, req.Params)
	case "create_page":
		return a.createPage(ctx, client, req.Params)
	case "update_page":
		return a.updatePage(ctx, client, req.Params)
	case "query_database":
		return a.queryDatabase(ctx, client, req.Params)
	case "list_databases":
		return a.listDatabases(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("notion: unsupported action %q", req.Action)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("notion: parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("notion: missing token in credential")
	}
	return cred.Token, nil
}

func notionClient(token string) *http.Client {
	return &http.Client{
		Transport: &tokenTransport{token: token, base: http.DefaultTransport},
	}
}

type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	clone.Header.Set("Notion-Version", "2022-06-28")
	clone.Header.Set("Content-Type", "application/json")
	return t.base.RoundTrip(clone)
}

// ── search ────────────────────────────────────────────────────────────────────

func (a *NotionAdapter) search(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	payload := map[string]any{}
	if query, ok := params["query"].(string); ok && query != "" {
		payload["query"] = query
	}
	if filter, ok := params["filter"].(map[string]any); ok {
		payload["filter"] = filter
	}
	pageSize := 20
	if v, ok := paramInt(params, "page_size"); ok && v > 0 && v <= 100 {
		pageSize = v
	}
	payload["page_size"] = pageSize

	var resp struct {
		Results []struct {
			ID         string `json:"id"`
			Object     string `json:"object"` // "page" or "database"
			URL        string `json:"url"`
			Properties map[string]any `json:"properties"`
		} `json:"results"`
	}
	if err := notionPOST(ctx, client, "search", payload, &resp); err != nil {
		return nil, fmt.Errorf("notion search: %w", err)
	}

	type searchItem struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Title  string `json:"title,omitempty"`
		URL    string `json:"url"`
	}
	items := make([]searchItem, 0, len(resp.Results))
	for _, r := range resp.Results {
		items = append(items, searchItem{
			ID:     r.ID,
			Object: r.Object,
			Title:  flattenTitle(r.Properties),
			URL:    r.URL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d search result(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_page ──────────────────────────────────────────────────────────────────

func (a *NotionAdapter) getPage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	pageID, _ := params["page_id"].(string)
	if pageID == "" {
		return nil, fmt.Errorf("notion get_page: page_id is required")
	}

	var resp struct {
		ID         string         `json:"id"`
		Object     string         `json:"object"`
		URL        string         `json:"url"`
		CreatedTime string        `json:"created_time"`
		Properties map[string]any `json:"properties"`
		Archived   bool           `json:"archived"`
	}
	if err := notionGET(ctx, client, "pages/"+pageID, &resp); err != nil {
		return nil, fmt.Errorf("notion get_page: %w", err)
	}

	result := map[string]any{
		"id":           resp.ID,
		"url":          resp.URL,
		"created_time": resp.CreatedTime,
		"archived":     resp.Archived,
		"properties":   flattenProperties(resp.Properties),
	}
	return &adapters.Result{
		Summary: format.Summary("Page %s", resp.ID),
		Data:    result,
	}, nil
}

// ── create_page ───────────────────────────────────────────────────────────────

func (a *NotionAdapter) createPage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	parent, ok := params["parent"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("notion create_page: parent is required")
	}
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("notion create_page: properties is required")
	}

	payload := map[string]any{
		"parent":     parent,
		"properties": properties,
	}
	if children, ok := params["children"].([]any); ok {
		payload["children"] = children
	}

	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := notionPOST(ctx, client, "pages", payload, &resp); err != nil {
		return nil, fmt.Errorf("notion create_page: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Created page %s", resp.ID),
		Data:    map[string]any{"id": resp.ID, "url": resp.URL},
	}, nil
}

// ── update_page ───────────────────────────────────────────────────────────────

func (a *NotionAdapter) updatePage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	pageID, _ := params["page_id"].(string)
	if pageID == "" {
		return nil, fmt.Errorf("notion update_page: page_id is required")
	}
	properties, ok := params["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("notion update_page: properties is required")
	}

	payload := map[string]any{
		"properties": properties,
	}
	if archived, ok := params["archived"].(bool); ok {
		payload["archived"] = archived
	}

	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := notionPATCH(ctx, client, "pages/"+pageID, payload, &resp); err != nil {
		return nil, fmt.Errorf("notion update_page: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Updated page %s", resp.ID),
		Data:    map[string]any{"id": resp.ID, "url": resp.URL},
	}, nil
}

// ── query_database ────────────────────────────────────────────────────────────

func (a *NotionAdapter) queryDatabase(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	dbID, _ := params["database_id"].(string)
	if dbID == "" {
		return nil, fmt.Errorf("notion query_database: database_id is required")
	}

	payload := map[string]any{}
	if filter, ok := params["filter"].(map[string]any); ok {
		payload["filter"] = filter
	}
	if sorts, ok := params["sorts"].([]any); ok {
		payload["sorts"] = sorts
	}
	pageSize := 50
	if v, ok := paramInt(params, "page_size"); ok && v > 0 && v <= 100 {
		pageSize = v
	}
	payload["page_size"] = pageSize

	var resp struct {
		Results []struct {
			ID         string         `json:"id"`
			URL        string         `json:"url"`
			Properties map[string]any `json:"properties"`
		} `json:"results"`
	}
	if err := notionPOST(ctx, client, "databases/"+dbID+"/query", payload, &resp); err != nil {
		return nil, fmt.Errorf("notion query_database: %w", err)
	}

	type rowItem struct {
		ID         string         `json:"id"`
		URL        string         `json:"url"`
		Properties map[string]any `json:"properties"`
	}
	items := make([]rowItem, 0, len(resp.Results))
	for _, r := range resp.Results {
		items = append(items, rowItem{
			ID:         r.ID,
			URL:        r.URL,
			Properties: flattenProperties(r.Properties),
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d row(s) from database %s", len(items), dbID),
		Data:    items,
	}, nil
}

// ── list_databases ────────────────────────────────────────────────────────────

func (a *NotionAdapter) listDatabases(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	pageSize := 20
	if v, ok := paramInt(params, "page_size"); ok && v > 0 && v <= 100 {
		pageSize = v
	}

	payload := map[string]any{
		"filter": map[string]any{
			"property": "object",
			"value":    "database",
		},
		"page_size": pageSize,
	}

	var resp struct {
		Results []struct {
			ID    string `json:"id"`
			URL   string `json:"url"`
			Title []struct {
				PlainText string `json:"plain_text"`
			} `json:"title"`
		} `json:"results"`
	}
	if err := notionPOST(ctx, client, "search", payload, &resp); err != nil {
		return nil, fmt.Errorf("notion list_databases: %w", err)
	}

	type dbItem struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	items := make([]dbItem, 0, len(resp.Results))
	for _, r := range resp.Results {
		title := ""
		if len(r.Title) > 0 {
			title = r.Title[0].PlainText
		}
		items = append(items, dbItem{
			ID:    r.ID,
			Title: format.SanitizeText(title, format.MaxFieldLen),
			URL:   r.URL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d database(s)", len(items)),
		Data:    items,
	}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

const notionAPIBase = "https://api.notion.com/v1/"

func notionGET(ctx context.Context, client *http.Client, path string, out any) error {
	apiURL := notionAPIBase + path
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

func notionPOST(ctx context.Context, client *http.Client, path string, payload any, out any) error {
	return notionWrite(ctx, client, http.MethodPost, path, payload, out)
}

func notionPATCH(ctx context.Context, client *http.Client, path string, payload any, out any) error {
	return notionWrite(ctx, client, http.MethodPatch, path, payload, out)
}

func notionWrite(ctx context.Context, client *http.Client, method, path string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	apiURL := notionAPIBase + path
	req, err := http.NewRequestWithContext(ctx, method, apiURL, bytes.NewReader(b))
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
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

// ── Property helpers ──────────────────────────────────────────────────────────

// flattenTitle extracts the page title from Notion's properties structure.
func flattenTitle(properties map[string]any) string {
	for _, v := range properties {
		prop, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if prop["type"] != "title" {
			continue
		}
		titleArr, ok := prop["title"].([]any)
		if !ok || len(titleArr) == 0 {
			continue
		}
		if first, ok := titleArr[0].(map[string]any); ok {
			if pt, ok := first["plain_text"].(string); ok {
				return format.SanitizeText(pt, format.MaxFieldLen)
			}
		}
	}
	return ""
}

// flattenProperties simplifies Notion's verbose property structure into readable key-value pairs.
func flattenProperties(properties map[string]any) map[string]any {
	out := make(map[string]any, len(properties))
	for name, v := range properties {
		prop, ok := v.(map[string]any)
		if !ok {
			out[name] = v
			continue
		}
		propType, _ := prop["type"].(string)
		switch propType {
		case "title":
			if arr, ok := prop["title"].([]any); ok && len(arr) > 0 {
				if first, ok := arr[0].(map[string]any); ok {
					out[name] = format.SanitizeText(first["plain_text"].(string), format.MaxFieldLen)
					continue
				}
			}
			out[name] = ""
		case "rich_text":
			if arr, ok := prop["rich_text"].([]any); ok && len(arr) > 0 {
				if first, ok := arr[0].(map[string]any); ok {
					if pt, ok := first["plain_text"].(string); ok {
						out[name] = format.SanitizeText(pt, format.MaxFieldLen)
						continue
					}
				}
			}
			out[name] = ""
		case "number":
			out[name] = prop["number"]
		case "select":
			if sel, ok := prop["select"].(map[string]any); ok {
				out[name] = sel["name"]
			} else {
				out[name] = nil
			}
		case "multi_select":
			if arr, ok := prop["multi_select"].([]any); ok {
				names := make([]string, 0, len(arr))
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						if n, ok := m["name"].(string); ok {
							names = append(names, n)
						}
					}
				}
				out[name] = names
			}
		case "checkbox":
			out[name] = prop["checkbox"]
		case "date":
			if d, ok := prop["date"].(map[string]any); ok {
				out[name] = d["start"]
			} else {
				out[name] = nil
			}
		case "url":
			out[name] = prop["url"]
		case "email":
			out[name] = prop["email"]
		default:
			out[name] = propType
		}
	}
	return out
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
