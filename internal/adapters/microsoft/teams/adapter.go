package teams

import (
	"context"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/microsoft"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := microsoft.ExtractToken(req.Credential)
	if err != nil {
		return nil, fmt.Errorf("teams: %w", err)
	}

	switch req.Action {
	case "send_message":
		return a.sendMessage(ctx, token, req.Params)
	default:
		return nil, fmt.Errorf("teams: unsupported action %q", req.Action)
	}
}

func (a *Adapter) sendMessage(ctx context.Context, token string, params map[string]any) (*adapters.Result, error) {
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

	endpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/teams/%s/channels/%s/messages", teamID, channelID)

	payload := map[string]any{
		"body": map[string]string{
			"contentType": "html",
			"content":     content,
		},
	}

	var out struct {
		ID string `json:"id"`
	}

	if err := microsoft.GraphPOST(ctx, token, endpoint, payload, &out); err != nil {
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
