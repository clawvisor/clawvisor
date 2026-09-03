package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/notify"
)

// installScopes are the bot scopes requested at install time.
//
//	chat:write         post approval prompts
//	chat:write.public  post to a public channel without joining it first
//	channels:read      list public channels for the picker
//	groups:read        list private channels the bot has been invited to
//	users:read         resolve Slack user IDs to names for the allowlist UI
//
// Deliberately excluded: any read scope for message content. Clawvisor never
// needs to read the channel, and requesting it would make the install prompt
// far more alarming than the feature warrants.
var installScopes = []string{
	"chat:write",
	"chat:write.public",
	"channels:read",
	"groups:read",
	"users:read",
}

// AppCredentials holds the Clawvisor Slack app's OAuth identity. A single
// app is installed into many workspaces, so these are deployment-wide, not
// per-user.
type AppCredentials struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Configured reports whether enough is set to run the install flow.
func (c AppCredentials) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// InstallURL builds the Slack authorize URL for an install. state must be a
// single-use, unguessable value bound to the initiating user's session — it
// is the only thing preventing an attacker from tricking a signed-in user
// into attaching the attacker's workspace to their account.
func (c AppCredentials) InstallURL(state string) (string, error) {
	if !c.Configured() {
		return "", errors.New("slack: app credentials not configured")
	}
	if state == "" {
		return "", errors.New("slack: state is required")
	}
	q := url.Values{
		"client_id":    {c.ClientID},
		"scope":        {strings.Join(installScopes, ",")},
		"redirect_uri": {c.RedirectURL},
		"state":        {state},
	}
	return "https://slack.com/oauth/v2/authorize?" + q.Encode(), nil
}

// SlackInstallURL returns the authorize URL for an install.
// Implements notify.SlackInstaller.
func (n *Notifier) SlackInstallURL(state string) (string, error) {
	return n.creds.InstallURL(state)
}

// CompleteSlackInstall trades an OAuth code for a workspace bot token. It
// deliberately does not persist anything: the caller pairs the install with a
// channel choice before writing a config, so an abandoned install leaves no
// half-configured row behind.
// Implements notify.SlackInstaller.
func (n *Notifier) CompleteSlackInstall(ctx context.Context, code string) (notify.SlackInstall, error) {
	creds := n.creds
	if !creds.Configured() {
		return notify.SlackInstall{}, errors.New("slack: app credentials not configured")
	}

	form := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"code":          {code},
		"redirect_uri":  {creds.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return notify.SlackInstall{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		Team        struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		AuthedUser struct {
			ID string `json:"id"`
		} `json:"authed_user"`
	}
	if err := n.doJSON(req, &out); err != nil {
		return notify.SlackInstall{}, err
	}
	if !out.OK {
		return notify.SlackInstall{}, fmt.Errorf("slack: oauth exchange failed: %s", out.Error)
	}
	if out.AccessToken == "" {
		return notify.SlackInstall{}, errors.New("slack: oauth exchange returned no bot token")
	}

	return notify.SlackInstall{
		BotToken:        out.AccessToken,
		TeamID:          out.Team.ID,
		TeamName:        out.Team.Name,
		InstallerUserID: out.AuthedUser.ID,
	}, nil
}

// ListSlackChannels enumerates channels the app could post to, for the picker.
//
// Slack paginates this endpoint and a large workspace has thousands of
// channels, so this walks a bounded number of pages rather than the whole
// workspace — the picker filters client-side over what it gets.
func (n *Notifier) ListSlackChannels(ctx context.Context, userID string) ([]notify.SlackChannel, error) {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	botToken := cfg.BotToken

	const (
		perPage  = 200
		maxPages = 10
		maxTotal = perPage * maxPages
	)

	var all []notify.SlackChannel
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"types":            {"public_channel,private_channel"},
			"exclude_archived": {"true"},
			"limit":            {fmt.Sprint(perPage)},
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://slack.com/api/conversations.list?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+botToken)

		var out struct {
			OK       bool                  `json:"ok"`
			Error    string                `json:"error"`
			Channels []notify.SlackChannel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := n.doJSON(req, &out); err != nil {
			return nil, err
		}
		if !out.OK {
			return nil, fmt.Errorf("slack: conversations.list: %s", out.Error)
		}

		all = append(all, out.Channels...)
		cursor = out.Meta.NextCursor
		if cursor == "" || len(all) >= maxTotal {
			break
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// LookupSlackUser resolves a Slack user ID to a display name so the allowlist UI
// can show something human-readable.
func (n *Notifier) LookupSlackUser(ctx context.Context, userID, slackUserID string) (string, error) {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return "", err
	}
	botToken := cfg.BotToken

	q := url.Values{"user": {slackUserID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://slack.com/api/users.info?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := n.doJSON(req, &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("slack: users.info: %s", out.Error)
	}
	for _, cand := range []string{out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.Name} {
		if cand != "" {
			return cand, nil
		}
	}
	return slackUserID, nil
}
