// Package slack implements the Clawvisor adapter for Slack.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
)

const serviceID = "slack"

// storedCredential holds a Slack bot token.
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // e.g. "xoxb-..."
}

// SlackAdapter implements adapters.Adapter for Slack.
type SlackAdapter struct{}

func New() *SlackAdapter { return &SlackAdapter{} }

func (a *SlackAdapter) ServiceID() string { return serviceID }

func (a *SlackAdapter) SupportedActions() []string {
	return []string{
		"list_channels", "get_channel", "list_messages",
		"send_message", "search_messages", "list_users",
	}
}

func (a *SlackAdapter) OAuthConfig() *oauth2.Config                          { return nil }
func (a *SlackAdapter) RequiredScopes() []string                             { return nil }
func (a *SlackAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error)  {
	return nil, fmt.Errorf("slack: OAuth token exchange not supported — use API key activation")
}

func (a *SlackAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("slack: invalid credential: %w", err)
	}
	if cred.Token == "" {
		return fmt.Errorf("slack: credential missing token")
	}
	return nil
}

func (a *SlackAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, err
	}
	client := slackClient(token)

	switch req.Action {
	case "list_channels":
		return a.listChannels(ctx, client, req.Params)
	case "get_channel":
		return a.getChannel(ctx, client, req.Params)
	case "list_messages":
		return a.listMessages(ctx, client, req.Params)
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	case "search_messages":
		return a.searchMessages(ctx, client, req.Params)
	case "list_users":
		return a.listUsers(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("slack: unsupported action %q", req.Action)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("slack: parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("slack: missing token in credential")
	}
	return cred.Token, nil
}

func slackClient(token string) *http.Client {
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
	clone.Header.Set("Content-Type", "application/json; charset=utf-8")
	return t.base.RoundTrip(clone)
}

// slackResponse wraps the common Slack API envelope.
type slackResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"-"` // filled by caller
}

// ── list_channels ─────────────────────────────────────────────────────────────

func (a *SlackAdapter) listChannels(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	q.Set("exclude_archived", "true")
	limit := 100
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 1000 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if types, ok := params["types"].(string); ok && types != "" {
		q.Set("types", types)
	}

	var resp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
		Channels []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Topic      struct{ Value string `json:"value"` } `json:"topic"`
			Purpose    struct{ Value string `json:"value"` } `json:"purpose"`
			NumMembers int    `json:"num_members"`
			IsPrivate  bool   `json:"is_private"`
			IsArchived bool   `json:"is_archived"`
		} `json:"channels"`
	}
	if err := slackGET(ctx, client, "conversations.list", q, &resp); err != nil {
		return nil, fmt.Errorf("slack list_channels: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack list_channels: %s", resp.Error)
	}

	type channelItem struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Topic      string `json:"topic,omitempty"`
		Purpose    string `json:"purpose,omitempty"`
		NumMembers int    `json:"num_members"`
		IsPrivate  bool   `json:"is_private"`
	}
	items := make([]channelItem, 0, len(resp.Channels))
	for _, ch := range resp.Channels {
		items = append(items, channelItem{
			ID:         ch.ID,
			Name:       ch.Name,
			Topic:      format.SanitizeText(ch.Topic.Value, format.MaxSnippetLen),
			Purpose:    format.SanitizeText(ch.Purpose.Value, format.MaxSnippetLen),
			NumMembers: ch.NumMembers,
			IsPrivate:  ch.IsPrivate,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d channel(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_channel ───────────────────────────────────────────────────────────────

func (a *SlackAdapter) getChannel(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	channel, _ := params["channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("slack get_channel: channel is required")
	}

	q := url.Values{}
	q.Set("channel", channel)

	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
		Channel struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Topic      struct{ Value string `json:"value"` } `json:"topic"`
			Purpose    struct{ Value string `json:"value"` } `json:"purpose"`
			NumMembers int    `json:"num_members"`
			IsPrivate  bool   `json:"is_private"`
			IsArchived bool   `json:"is_archived"`
			Created    int64  `json:"created"`
		} `json:"channel"`
	}
	if err := slackGET(ctx, client, "conversations.info", q, &resp); err != nil {
		return nil, fmt.Errorf("slack get_channel: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack get_channel: %s", resp.Error)
	}

	result := map[string]any{
		"id":          resp.Channel.ID,
		"name":        resp.Channel.Name,
		"topic":       format.SanitizeText(resp.Channel.Topic.Value, format.MaxSnippetLen),
		"purpose":     format.SanitizeText(resp.Channel.Purpose.Value, format.MaxSnippetLen),
		"num_members": resp.Channel.NumMembers,
		"is_private":  resp.Channel.IsPrivate,
		"is_archived": resp.Channel.IsArchived,
	}
	return &adapters.Result{
		Summary: format.Summary("Channel #%s (%d members)", resp.Channel.Name, resp.Channel.NumMembers),
		Data:    result,
	}, nil
}

// ── list_messages ─────────────────────────────────────────────────────────────

func (a *SlackAdapter) listMessages(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	channel, _ := params["channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("slack list_messages: channel is required")
	}

	q := url.Values{}
	q.Set("channel", channel)
	limit := 50
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 1000 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if oldest, ok := params["oldest"].(string); ok && oldest != "" {
		q.Set("oldest", oldest)
	}
	if latest, ok := params["latest"].(string); ok && latest != "" {
		q.Set("latest", latest)
	}

	var resp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
		Messages []struct {
			Type string `json:"type"`
			User string `json:"user"`
			Text string `json:"text"`
			TS   string `json:"ts"`
		} `json:"messages"`
	}
	if err := slackGET(ctx, client, "conversations.history", q, &resp); err != nil {
		return nil, fmt.Errorf("slack list_messages: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack list_messages: %s", resp.Error)
	}

	type messageItem struct {
		User string `json:"user"`
		Text string `json:"text"`
		TS   string `json:"ts"`
	}
	items := make([]messageItem, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		items = append(items, messageItem{
			User: m.User,
			Text: format.SanitizeText(m.Text, format.MaxBodyLen),
			TS:   m.TS,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d message(s) in %s", len(items), channel),
		Data:    items,
	}, nil
}

// ── send_message ──────────────────────────────────────────────────────────────

func (a *SlackAdapter) sendMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	channel, _ := params["channel"].(string)
	text, _ := params["text"].(string)
	if channel == "" || text == "" {
		return nil, fmt.Errorf("slack send_message: channel and text are required")
	}

	payload := map[string]any{
		"channel": channel,
		"text":    text,
	}
	if threadTS, ok := params["thread_ts"].(string); ok && threadTS != "" {
		payload["thread_ts"] = threadTS
	}

	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := slackPOST(ctx, client, "chat.postMessage", payload, &resp); err != nil {
		return nil, fmt.Errorf("slack send_message: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack send_message: %s", resp.Error)
	}

	return &adapters.Result{
		Summary: format.Summary("Message sent to %s", channel),
		Data:    map[string]any{"channel": resp.Channel, "ts": resp.TS},
	}, nil
}

// ── search_messages ───────────────────────────────────────────────────────────

func (a *SlackAdapter) searchMessages(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("slack search_messages: query is required")
	}

	q := url.Values{}
	q.Set("query", query)
	count := 20
	if v, ok := paramInt(params, "count"); ok && v > 0 && v <= 100 {
		count = v
	}
	q.Set("count", fmt.Sprintf("%d", count))

	var resp struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
		Messages struct {
			Total  int `json:"total"`
			Matches []struct {
				Text    string `json:"text"`
				TS      string `json:"ts"`
				Channel struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"channel"`
				User string `json:"username"`
			} `json:"matches"`
		} `json:"messages"`
	}
	if err := slackGET(ctx, client, "search.messages", q, &resp); err != nil {
		return nil, fmt.Errorf("slack search_messages: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack search_messages: %s", resp.Error)
	}

	type matchItem struct {
		Text        string `json:"text"`
		TS          string `json:"ts"`
		ChannelID   string `json:"channel_id"`
		ChannelName string `json:"channel_name"`
		User        string `json:"user"`
	}
	items := make([]matchItem, 0, len(resp.Messages.Matches))
	for _, m := range resp.Messages.Matches {
		items = append(items, matchItem{
			Text:        format.SanitizeText(m.Text, format.MaxSnippetLen),
			TS:          m.TS,
			ChannelID:   m.Channel.ID,
			ChannelName: m.Channel.Name,
			User:        m.User,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d of %d result(s) for %q", len(items), resp.Messages.Total, query),
		Data:    items,
	}, nil
}

// ── list_users ────────────────────────────────────────────────────────────────

func (a *SlackAdapter) listUsers(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	limit := 100
	if v, ok := paramInt(params, "limit"); ok && v > 0 && v <= 1000 {
		limit = v
	}
	q.Set("limit", fmt.Sprintf("%d", limit))

	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
		Members []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			IsBot    bool   `json:"is_bot"`
			Deleted  bool   `json:"deleted"`
			IsAdmin  bool   `json:"is_admin"`
			Profile  struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"members"`
	}
	if err := slackGET(ctx, client, "users.list", q, &resp); err != nil {
		return nil, fmt.Errorf("slack list_users: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack list_users: %s", resp.Error)
	}

	type userItem struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		RealName string `json:"real_name"`
		Email    string `json:"email,omitempty"`
		IsBot    bool   `json:"is_bot"`
		IsAdmin  bool   `json:"is_admin"`
	}
	items := make([]userItem, 0, len(resp.Members))
	for _, m := range resp.Members {
		if m.Deleted {
			continue
		}
		items = append(items, userItem{
			ID:       m.ID,
			Name:     m.Name,
			RealName: format.SanitizeText(m.RealName, format.MaxFieldLen),
			Email:    m.Profile.Email,
			IsBot:    m.IsBot,
			IsAdmin:  m.IsAdmin,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d user(s)", len(items)),
		Data:    items,
	}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

const slackAPIBase = "https://slack.com/api/"

func slackGET(ctx context.Context, client *http.Client, method string, q url.Values, out any) error {
	apiURL := slackAPIBase + method
	if len(q) > 0 {
		apiURL += "?" + q.Encode()
	}
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

func slackPOST(ctx context.Context, client *http.Client, method string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	apiURL := slackAPIBase + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(b))
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
