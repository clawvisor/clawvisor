package teams

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/microsoft"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Adapter handles Go override actions for Microsoft Teams.
type Adapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

// New creates a Teams adapter with the given OAuth credential provider
// for automatic token refresh.
func New(provider adapters.OAuthCredentialProvider) *Adapter {
	return &Adapter{oauthProvider: provider}
}

func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := microsoft.HTTPClient(ctx, req.Credential, a.oauthProvider)
	if err != nil {
		return nil, fmt.Errorf("teams: %w", err)
	}

	switch req.Action {
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("teams: unsupported action %q", req.Action)
	}
}

func (a *Adapter) sendMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	teamID, _ := params["team_id"].(string)
	channelID, _ := params["channel_id"].(string)
	content, _ := params["content"].(string)

	if teamID == "" {
		return nil, fmt.Errorf("teams send_message: team_id is required")
	}
	if channelID == "" {
		return nil, fmt.Errorf("teams send_message: channel_id is required")
	}
	if content == "" {
		return nil, fmt.Errorf("teams send_message: content is required")
	}

	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/teams/%s/channels/%s/messages", url.PathEscape(teamID), url.PathEscape(channelID))

	payload := map[string]any{
		"body": map[string]string{
			"contentType": "html",
			"content":     content,
		},
	}

	var out struct {
		ID string `json:"id"`
	}

	if err := microsoft.GraphPOST(ctx, client, endpoint, payload, &out); err != nil {
		return nil, fmt.Errorf("teams send_message: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Message sent to channel %s", channelID),
		Data: map[string]any{
			"id":         out.ID,
			"team_id":    teamID,
			"channel_id": channelID,
		},
	}, nil
}
