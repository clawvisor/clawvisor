// Package linear implements the Clawvisor adapter for Linear.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
)

const serviceID = "linear"

// storedCredential holds a Linear personal API key.
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // e.g. "lin_api_..."
}

// LinearAdapter implements adapters.Adapter for Linear.
type LinearAdapter struct{}

func New() *LinearAdapter { return &LinearAdapter{} }

func (a *LinearAdapter) ServiceID() string { return serviceID }

func (a *LinearAdapter) SupportedActions() []string {
	return []string{
		"list_issues", "get_issue", "create_issue", "update_issue",
		"add_comment", "list_teams", "list_projects", "search_issues",
	}
}

func (a *LinearAdapter) OAuthConfig() *oauth2.Config                         { return nil }
func (a *LinearAdapter) RequiredScopes() []string                            { return nil }
func (a *LinearAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("linear: OAuth token exchange not supported — use API key activation")
}

func (a *LinearAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("linear: invalid credential: %w", err)
	}
	if cred.Token == "" {
		return fmt.Errorf("linear: credential missing token")
	}
	return nil
}

func (a *LinearAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, err
	}
	client := linearClient(token)

	switch req.Action {
	case "list_issues":
		return a.listIssues(ctx, client, req.Params)
	case "get_issue":
		return a.getIssue(ctx, client, req.Params)
	case "create_issue":
		return a.createIssue(ctx, client, req.Params)
	case "update_issue":
		return a.updateIssue(ctx, client, req.Params)
	case "add_comment":
		return a.addComment(ctx, client, req.Params)
	case "list_teams":
		return a.listTeams(ctx, client)
	case "list_projects":
		return a.listProjects(ctx, client, req.Params)
	case "search_issues":
		return a.searchIssues(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("linear: unsupported action %q", req.Action)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("linear: parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("linear: missing token in credential")
	}
	return cred.Token, nil
}

func linearClient(token string) *http.Client {
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
	// Linear uses the API key directly as the Authorization header value (no Bearer prefix).
	clone.Header.Set("Authorization", t.token)
	clone.Header.Set("Content-Type", "application/json")
	return t.base.RoundTrip(clone)
}

// ── list_issues ───────────────────────────────────────────────────────────────

func (a *LinearAdapter) listIssues(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	first := 50
	if v, ok := paramInt(params, "first"); ok && v > 0 && v <= 250 {
		first = v
	}

	filterParts := ""
	if teamID, ok := params["team_id"].(string); ok && teamID != "" {
		filterParts += fmt.Sprintf(`, team: {id: {eq: %q}}`, teamID)
	}
	if state, ok := params["state"].(string); ok && state != "" {
		filterParts += fmt.Sprintf(`, state: {name: {eq: %q}}`, state)
	}
	if assigneeID, ok := params["assignee_id"].(string); ok && assigneeID != "" {
		filterParts += fmt.Sprintf(`, assignee: {id: {eq: %q}}`, assigneeID)
	}

	filter := ""
	if filterParts != "" {
		filter = fmt.Sprintf("filter: {%s}", filterParts[2:]) // trim leading ", "
	}

	query := fmt.Sprintf(`query {
		issues(first: %d %s) {
			nodes {
				id
				identifier
				title
				state { name }
				priority
				assignee { name }
				team { name key }
				createdAt
				url
			}
		}
	}`, first, commaPrefix(filter))

	var resp struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Title      string `json:"title"`
					State      struct{ Name string `json:"name"` } `json:"state"`
					Priority   int    `json:"priority"`
					Assignee   *struct{ Name string `json:"name"` } `json:"assignee"`
					Team       struct {
						Name string `json:"name"`
						Key  string `json:"key"`
					} `json:"team"`
					CreatedAt string `json:"createdAt"`
					URL        string `json:"url"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, nil, &resp); err != nil {
		return nil, fmt.Errorf("linear list_issues: %w", err)
	}

	type issueItem struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		State      string `json:"state"`
		Priority   int    `json:"priority"`
		Assignee   string `json:"assignee,omitempty"`
		Team       string `json:"team"`
		CreatedAt  string `json:"created_at"`
		URL        string `json:"url"`
	}
	items := make([]issueItem, 0, len(resp.Data.Issues.Nodes))
	for _, n := range resp.Data.Issues.Nodes {
		assignee := ""
		if n.Assignee != nil {
			assignee = n.Assignee.Name
		}
		items = append(items, issueItem{
			ID:         n.ID,
			Identifier: n.Identifier,
			Title:      format.SanitizeText(n.Title, format.MaxFieldLen),
			State:      n.State.Name,
			Priority:   n.Priority,
			Assignee:   assignee,
			Team:       n.Team.Name,
			CreatedAt:  n.CreatedAt,
			URL:        n.URL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d issue(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_issue ─────────────────────────────────────────────────────────────────

func (a *LinearAdapter) getIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	issueID, _ := params["issue_id"].(string)
	if issueID == "" {
		return nil, fmt.Errorf("linear get_issue: issue_id is required")
	}

	query := `query($id: String!) {
		issue(id: $id) {
			id
			identifier
			title
			description
			state { name }
			priority
			assignee { name }
			team { name key }
			project { name }
			createdAt
			url
		}
	}`
	vars := map[string]any{"id": issueID}

	var resp struct {
		Data struct {
			Issue struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				State       struct{ Name string `json:"name"` } `json:"state"`
				Priority    int    `json:"priority"`
				Assignee    *struct{ Name string `json:"name"` } `json:"assignee"`
				Team        struct {
					Name string `json:"name"`
					Key  string `json:"key"`
				} `json:"team"`
				Project   *struct{ Name string `json:"name"` } `json:"project"`
				CreatedAt string `json:"createdAt"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, vars, &resp); err != nil {
		return nil, fmt.Errorf("linear get_issue: %w", err)
	}

	issue := resp.Data.Issue
	assignee := ""
	if issue.Assignee != nil {
		assignee = issue.Assignee.Name
	}
	project := ""
	if issue.Project != nil {
		project = issue.Project.Name
	}
	result := map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       format.SanitizeText(issue.Title, format.MaxFieldLen),
		"description": format.SanitizeText(issue.Description, format.MaxBodyLen),
		"state":       issue.State.Name,
		"priority":    issue.Priority,
		"assignee":    assignee,
		"team":        issue.Team.Name,
		"project":     project,
		"created_at":  issue.CreatedAt,
		"url":         issue.URL,
	}
	return &adapters.Result{
		Summary: format.Summary("%s: %s [%s]", issue.Identifier, issue.Title, issue.State.Name),
		Data:    result,
	}, nil
}

// ── create_issue ──────────────────────────────────────────────────────────────

func (a *LinearAdapter) createIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	title, _ := params["title"].(string)
	teamID, _ := params["team_id"].(string)
	if title == "" || teamID == "" {
		return nil, fmt.Errorf("linear create_issue: title and team_id are required")
	}

	query := `mutation($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue {
				id
				identifier
				title
				url
			}
		}
	}`

	input := map[string]any{
		"title":  title,
		"teamId": teamID,
	}
	if desc, ok := params["description"].(string); ok {
		input["description"] = desc
	}
	if priority, ok := paramInt(params, "priority"); ok {
		input["priority"] = priority
	}
	if assigneeID, ok := params["assignee_id"].(string); ok && assigneeID != "" {
		input["assigneeId"] = assigneeID
	}
	if stateID, ok := params["state_id"].(string); ok && stateID != "" {
		input["stateId"] = stateID
	}

	var resp struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Title      string `json:"title"`
					URL        string `json:"url"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, map[string]any{"input": input}, &resp); err != nil {
		return nil, fmt.Errorf("linear create_issue: %w", err)
	}

	created := resp.Data.IssueCreate.Issue
	return &adapters.Result{
		Summary: format.Summary("Created %s: %s", created.Identifier, created.Title),
		Data:    map[string]any{"id": created.ID, "identifier": created.Identifier, "title": created.Title, "url": created.URL},
	}, nil
}

// ── update_issue ──────────────────────────────────────────────────────────────

func (a *LinearAdapter) updateIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	issueID, _ := params["issue_id"].(string)
	if issueID == "" {
		return nil, fmt.Errorf("linear update_issue: issue_id is required")
	}

	query := `mutation($id: String!, $input: IssueUpdateInput!) {
		issueUpdate(id: $id, input: $input) {
			success
			issue {
				id
				identifier
				title
				url
			}
		}
	}`

	input := map[string]any{}
	if title, ok := params["title"].(string); ok {
		input["title"] = title
	}
	if desc, ok := params["description"].(string); ok {
		input["description"] = desc
	}
	if stateID, ok := params["state_id"].(string); ok && stateID != "" {
		input["stateId"] = stateID
	}
	if priority, ok := paramInt(params, "priority"); ok {
		input["priority"] = priority
	}
	if assigneeID, ok := params["assignee_id"].(string); ok && assigneeID != "" {
		input["assigneeId"] = assigneeID
	}

	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
				Issue   struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Title      string `json:"title"`
					URL        string `json:"url"`
				} `json:"issue"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, map[string]any{"id": issueID, "input": input}, &resp); err != nil {
		return nil, fmt.Errorf("linear update_issue: %w", err)
	}

	updated := resp.Data.IssueUpdate.Issue
	return &adapters.Result{
		Summary: format.Summary("Updated %s: %s", updated.Identifier, updated.Title),
		Data:    map[string]any{"id": updated.ID, "identifier": updated.Identifier, "title": updated.Title, "url": updated.URL},
	}, nil
}

// ── add_comment ───────────────────────────────────────────────────────────────

func (a *LinearAdapter) addComment(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	issueID, _ := params["issue_id"].(string)
	body, _ := params["body"].(string)
	if issueID == "" || body == "" {
		return nil, fmt.Errorf("linear add_comment: issue_id and body are required")
	}

	query := `mutation($input: CommentCreateInput!) {
		commentCreate(input: $input) {
			success
			comment {
				id
				url
			}
		}
	}`

	var resp struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
				Comment struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"comment"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	vars := map[string]any{
		"input": map[string]any{
			"issueId": issueID,
			"body":    body,
		},
	}
	if err := graphqlDo(ctx, client, query, vars, &resp); err != nil {
		return nil, fmt.Errorf("linear add_comment: %w", err)
	}

	comment := resp.Data.CommentCreate.Comment
	return &adapters.Result{
		Summary: format.Summary("Added comment to issue"),
		Data:    map[string]any{"id": comment.ID, "url": comment.URL},
	}, nil
}

// ── list_teams ────────────────────────────────────────────────────────────────

func (a *LinearAdapter) listTeams(ctx context.Context, client *http.Client) (*adapters.Result, error) {
	query := `query {
		teams {
			nodes {
				id
				name
				key
			}
		}
	}`

	var resp struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Key  string `json:"key"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, nil, &resp); err != nil {
		return nil, fmt.Errorf("linear list_teams: %w", err)
	}

	type teamItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	items := make([]teamItem, 0, len(resp.Data.Teams.Nodes))
	for _, n := range resp.Data.Teams.Nodes {
		items = append(items, teamItem{ID: n.ID, Name: n.Name, Key: n.Key})
	}
	return &adapters.Result{
		Summary: format.Summary("%d team(s)", len(items)),
		Data:    items,
	}, nil
}

// ── list_projects ─────────────────────────────────────────────────────────────

func (a *LinearAdapter) listProjects(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	first := 50
	if v, ok := paramInt(params, "first"); ok && v > 0 && v <= 250 {
		first = v
	}

	query := fmt.Sprintf(`query {
		projects(first: %d) {
			nodes {
				id
				name
				state
				startDate
				targetDate
				url
			}
		}
	}`, first)

	var resp struct {
		Data struct {
			Projects struct {
				Nodes []struct {
					ID         string `json:"id"`
					Name       string `json:"name"`
					State      string `json:"state"`
					StartDate  string `json:"startDate"`
					TargetDate string `json:"targetDate"`
					URL        string `json:"url"`
				} `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, nil, &resp); err != nil {
		return nil, fmt.Errorf("linear list_projects: %w", err)
	}

	type projectItem struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		State      string `json:"state"`
		StartDate  string `json:"start_date,omitempty"`
		TargetDate string `json:"target_date,omitempty"`
		URL        string `json:"url"`
	}
	items := make([]projectItem, 0, len(resp.Data.Projects.Nodes))
	for _, n := range resp.Data.Projects.Nodes {
		items = append(items, projectItem{
			ID:         n.ID,
			Name:       format.SanitizeText(n.Name, format.MaxFieldLen),
			State:      n.State,
			StartDate:  n.StartDate,
			TargetDate: n.TargetDate,
			URL:        n.URL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d project(s)", len(items)),
		Data:    items,
	}, nil
}

// ── search_issues ─────────────────────────────────────────────────────────────

func (a *LinearAdapter) searchIssues(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	queryStr, _ := params["query"].(string)
	if queryStr == "" {
		return nil, fmt.Errorf("linear search_issues: query is required")
	}
	first := 20
	if v, ok := paramInt(params, "first"); ok && v > 0 && v <= 250 {
		first = v
	}

	query := fmt.Sprintf(`query($filter: IssueFilter!) {
		issues(first: %d, filter: $filter) {
			nodes {
				id
				identifier
				title
				state { name }
				priority
				assignee { name }
				team { name key }
				url
			}
		}
	}`, first)

	vars := map[string]any{
		"filter": map[string]any{
			"or": []map[string]any{
				{"title": map[string]any{"containsIgnoreCase": queryStr}},
				{"description": map[string]any{"containsIgnoreCase": queryStr}},
			},
		},
	}

	var resp struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Title      string `json:"title"`
					State      struct{ Name string `json:"name"` } `json:"state"`
					Priority   int    `json:"priority"`
					Assignee   *struct{ Name string `json:"name"` } `json:"assignee"`
					Team       struct {
						Name string `json:"name"`
						Key  string `json:"key"`
					} `json:"team"`
					URL string `json:"url"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := graphqlDo(ctx, client, query, vars, &resp); err != nil {
		return nil, fmt.Errorf("linear search_issues: %w", err)
	}

	type issueItem struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		State      string `json:"state"`
		Priority   int    `json:"priority"`
		Assignee   string `json:"assignee,omitempty"`
		Team       string `json:"team"`
		URL        string `json:"url"`
	}
	items := make([]issueItem, 0, len(resp.Data.Issues.Nodes))
	for _, n := range resp.Data.Issues.Nodes {
		assignee := ""
		if n.Assignee != nil {
			assignee = n.Assignee.Name
		}
		items = append(items, issueItem{
			ID:         n.ID,
			Identifier: n.Identifier,
			Title:      format.SanitizeText(n.Title, format.MaxFieldLen),
			State:      n.State.Name,
			Priority:   n.Priority,
			Assignee:   assignee,
			Team:       n.Team.Name,
			URL:        n.URL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d result(s) for %q", len(items), queryStr),
		Data:    items,
	}, nil
}

// ── GraphQL helper ────────────────────────────────────────────────────────────

const linearAPIURL = "https://api.linear.app/graphql"

func graphqlDo(ctx context.Context, client *http.Client, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, linearAPIURL, bytes.NewReader(b))
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

	// Check for GraphQL-level errors.
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", envelope.Errors[0].Message)
	}

	return json.Unmarshal(body, out)
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// commaPrefix returns ", <s>" if s is non-empty, else "".
func commaPrefix(s string) string {
	if s == "" {
		return ""
	}
	return ", " + s
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
