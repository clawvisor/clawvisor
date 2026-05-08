package outlook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/microsoft"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Adapter handles Go override actions for Microsoft Outlook.
type Adapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

// New creates an Outlook adapter with the given OAuth credential provider
// for automatic token refresh.
func New(provider adapters.OAuthCredentialProvider) *Adapter {
	return &Adapter{oauthProvider: provider}
}

func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := microsoft.HTTPClient(ctx, req.Credential, a.oauthProvider)
	if err != nil {
		return nil, fmt.Errorf("outlook: %w", err)
	}

	switch req.Action {
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	case "create_event":
		return a.createEvent(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("outlook: unsupported action %q", req.Action)
	}
}

func (a *Adapter) sendMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	subject, _ := params["subject"].(string)
	body, _ := params["body"].(string)

	if to == "" {
		return nil, fmt.Errorf("outlook send_message: to is required")
	}
	if subject == "" {
		return nil, fmt.Errorf("outlook send_message: subject is required")
	}

	toRecipients := []map[string]any{
		{
			"emailAddress": map[string]string{
				"address": to,
			},
		},
	}

	payload := map[string]any{
		"message": map[string]any{
			"subject": subject,
			"body": map[string]string{
				"contentType": "Text",
				"content":     body,
			},
			"toRecipients": toRecipients,
		},
		"saveToSentItems": "true",
	}

	if err := microsoft.GraphPOST(ctx, client, "https://graph.microsoft.com/v1.0/me/sendMail", payload, nil); err != nil {
		return nil, fmt.Errorf("outlook send_message: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Email sent to %s (subject: %q)", to, subject),
		Data: map[string]any{
			"to":      to,
			"subject": subject,
		},
	}, nil
}

func (a *Adapter) createEvent(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	subject, _ := params["subject"].(string)
	start, _ := params["start"].(string)
	end, _ := params["end"].(string)
	timezone, _ := params["timezone"].(string)
	if timezone == "" {
		timezone = "UTC"
	}
	locationStr, _ := params["location"].(string)
	bodyStr, _ := params["body"].(string)
	attendeesRaw := params["attendees"]

	if subject == "" {
		return nil, fmt.Errorf("outlook create_event: subject is required")
	}
	if start == "" {
		return nil, fmt.Errorf("outlook create_event: start is required")
	}
	if end == "" {
		return nil, fmt.Errorf("outlook create_event: end is required")
	}

	payload := map[string]any{
		"subject": subject,
		"start": map[string]string{
			"dateTime": start,
			"timeZone": timezone,
		},
		"end": map[string]string{
			"dateTime": end,
			"timeZone": timezone,
		},
	}

	if locationStr != "" {
		payload["location"] = map[string]string{
			"displayName": locationStr,
		}
	}

	if bodyStr != "" {
		payload["body"] = map[string]string{
			"contentType": "Text",
			"content":     bodyStr,
		}
	}

	if attendeesRaw != nil {
		var emails []string
		switch v := attendeesRaw.(type) {
		case string:
			if v != "" {
				emails = strings.Split(v, ",")
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && s != "" {
					emails = append(emails, s)
				}
			}
		case []string:
			emails = v
		}

		if len(emails) > 0 {
			var attendees []map[string]any
			for _, email := range emails {
				email = strings.TrimSpace(email)
				if email != "" {
					attendees = append(attendees, map[string]any{
						"emailAddress": map[string]string{
							"address": email,
						},
						"type": "required",
					})
				}
			}
			if len(attendees) > 0 {
				payload["attendees"] = attendees
			}
		}
	}

	var out struct {
		ID string `json:"id"`
	}

	if err := microsoft.GraphPOST(ctx, client, "https://graph.microsoft.com/v1.0/me/events", payload, &out); err != nil {
		return nil, fmt.Errorf("outlook create_event: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("Event created: %q", subject),
		Data: map[string]any{
			"id":        out.ID,
			"subject":   subject,
			"start":     start,
			"end":       end,
			"timezone":  timezone,
			"location":  locationStr,
			"body":      bodyStr,
			"attendees": payload["attendees"],
		},
	}, nil
}
