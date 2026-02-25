// Package github implements the Clawvisor adapter for GitHub.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
)

const serviceID = "github"

// storedCredential holds a GitHub personal access token.
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // e.g. "ghp_..."
}

// GitHubAdapter implements adapters.Adapter for GitHub.
// It uses a personal access token (not OAuth); users supply their own token
// via the dashboard.
type GitHubAdapter struct{}

func New() *GitHubAdapter { return &GitHubAdapter{} }

func (a *GitHubAdapter) ServiceID() string { return serviceID }

func (a *GitHubAdapter) SupportedActions() []string {
	return []string{
		"list_issues", "get_issue", "create_issue", "comment_issue",
		"list_prs", "get_pr",
		"list_repos", "search_code",
	}
}

// OAuthConfig returns nil — GitHub uses API keys, not OAuth.
func (a *GitHubAdapter) OAuthConfig() *oauth2.Config { return nil }

// RequiredScopes returns nil — GitHub uses API keys, not OAuth scopes.
func (a *GitHubAdapter) RequiredScopes() []string { return nil }

// CredentialFromToken is unused for API-key services.
func (a *GitHubAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("github: OAuth token exchange not supported — use API key activation")
}

func (a *GitHubAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("github: invalid credential: %w", err)
	}
	if cred.Token == "" {
		return fmt.Errorf("github: credential missing token")
	}
	return nil
}

func (a *GitHubAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	token, err := extractToken(req.Credential)
	if err != nil {
		return nil, err
	}
	client := githubClient(token)

	switch req.Action {
	case "list_issues":
		return a.listIssues(ctx, client, req.Params)
	case "get_issue":
		return a.getIssue(ctx, client, req.Params)
	case "create_issue":
		return a.createIssue(ctx, client, req.Params)
	case "comment_issue":
		return a.commentIssue(ctx, client, req.Params)
	case "list_prs":
		return a.listPRs(ctx, client, req.Params)
	case "get_pr":
		return a.getPR(ctx, client, req.Params)
	case "list_repos":
		return a.listRepos(ctx, client, req.Params)
	case "search_code":
		return a.searchCode(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("github: unsupported action %q", req.Action)
	}
}

func extractToken(credBytes []byte) (string, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", fmt.Errorf("github: parsing credential: %w", err)
	}
	if cred.Token == "" {
		return "", fmt.Errorf("github: missing token in credential")
	}
	return cred.Token, nil
}

// githubClient builds an *http.Client that injects the GitHub auth token.
func githubClient(token string) *http.Client {
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
	clone.Header.Set("Authorization", "token "+t.token)
	clone.Header.Set("Accept", "application/vnd.github.v3+json")
	clone.Header.Set("User-Agent", "clawvisor/1.0")
	return t.base.RoundTrip(clone)
}

// ── list_issues ───────────────────────────────────────────────────────────────

type issueItem struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	CreatedAt string   `json:"created_at"`
	URL       string   `json:"url"`
}

func (a *GitHubAdapter) listIssues(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github list_issues: %w", err)
	}
	state, _ := params["state"].(string)
	if state == "" {
		state = "open"
	}
	maxResults := 30
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 100 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", fmt.Sprintf("%d", maxResults))
	if labels, ok := params["labels"].(string); ok && labels != "" {
		q.Set("labels", labels)
	}
	if assignee, ok := params["assignee"].(string); ok && assignee != "" {
		q.Set("assignee", assignee)
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?%s",
		url.PathEscape(owner), url.PathEscape(repo), q.Encode())

	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		CreatedAt string `json:"created_at"`
		HTMLURL   string `json:"html_url"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		PullRequest *struct{} `json:"pull_request"` // present if issue is a PR
	}
	if err := apiGET(ctx, client, apiURL, &raw); err != nil {
		return nil, fmt.Errorf("github list_issues: %w", err)
	}

	items := make([]issueItem, 0, len(raw))
	for _, r := range raw {
		if r.PullRequest != nil {
			continue // skip PRs in issue list
		}
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		assignees := make([]string, 0, len(r.Assignees))
		for _, as := range r.Assignees {
			assignees = append(assignees, as.Login)
		}
		items = append(items, issueItem{
			Number:    r.Number,
			Title:     format.SanitizeText(r.Title, format.MaxFieldLen),
			State:     r.State,
			Labels:    labels,
			Assignees: assignees,
			CreatedAt: r.CreatedAt,
			URL:       r.HTMLURL,
		})
	}
	summary := format.Summary("%d %s issue(s) in %s/%s", len(items), state, owner, repo)
	return &adapters.Result{Summary: summary, Data: items}, nil
}

// ── get_issue ─────────────────────────────────────────────────────────────────

func (a *GitHubAdapter) getIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github get_issue: %w", err)
	}
	number, ok := paramInt(params, "number")
	if !ok {
		return nil, fmt.Errorf("github get_issue: number is required")
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d",
		url.PathEscape(owner), url.PathEscape(repo), number)
	var raw struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
	}
	if err := apiGET(ctx, client, apiURL, &raw); err != nil {
		return nil, fmt.Errorf("github get_issue: %w", err)
	}

	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(raw.Assignees))
	for _, as := range raw.Assignees {
		assignees = append(assignees, as.Login)
	}
	result := map[string]any{
		"number":     raw.Number,
		"title":      format.SanitizeText(raw.Title, format.MaxFieldLen),
		"body":       format.SanitizeText(raw.Body, format.MaxBodyLen),
		"state":      raw.State,
		"author":     raw.User.Login,
		"labels":     labels,
		"assignees":  assignees,
		"created_at": raw.CreatedAt,
		"url":        raw.HTMLURL,
	}
	return &adapters.Result{
		Summary: format.Summary("Issue #%d: %s [%s]", raw.Number, raw.Title, raw.State),
		Data:    result,
	}, nil
}

// ── create_issue ──────────────────────────────────────────────────────────────

func (a *GitHubAdapter) createIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github create_issue: %w", err)
	}
	title, _ := params["title"].(string)
	body, _ := params["body"].(string)
	if title == "" {
		return nil, fmt.Errorf("github create_issue: title is required")
	}

	payload := map[string]any{"title": title, "body": body}
	if labels, ok := params["labels"].([]any); ok {
		ss := make([]string, 0, len(labels))
		for _, l := range labels {
			if s, ok := l.(string); ok {
				ss = append(ss, s)
			}
		}
		payload["labels"] = ss
	}
	if assignees, ok := params["assignees"].([]any); ok {
		ss := make([]string, 0, len(assignees))
		for _, as := range assignees {
			if s, ok := as.(string); ok {
				ss = append(ss, s)
			}
		}
		payload["assignees"] = ss
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues",
		url.PathEscape(owner), url.PathEscape(repo))
	var created struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	}
	if err := apiWrite(ctx, client, http.MethodPost, apiURL, payload, &created); err != nil {
		return nil, fmt.Errorf("github create_issue: %w", err)
	}
	return &adapters.Result{
		Summary: format.Summary("Created issue #%d: %s", created.Number, created.Title),
		Data:    map[string]any{"number": created.Number, "title": created.Title, "url": created.HTMLURL},
	}, nil
}

// ── comment_issue ─────────────────────────────────────────────────────────────

func (a *GitHubAdapter) commentIssue(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github comment_issue: %w", err)
	}
	number, ok := paramInt(params, "number")
	if !ok {
		return nil, fmt.Errorf("github comment_issue: number is required")
	}
	body, _ := params["body"].(string)
	if body == "" {
		return nil, fmt.Errorf("github comment_issue: body is required")
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments",
		url.PathEscape(owner), url.PathEscape(repo), number)
	var created struct {
		ID      int    `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := apiWrite(ctx, client, http.MethodPost, apiURL, map[string]string{"body": body}, &created); err != nil {
		return nil, fmt.Errorf("github comment_issue: %w", err)
	}
	return &adapters.Result{
		Summary: format.Summary("Added comment to issue #%d", number),
		Data:    map[string]any{"comment_id": created.ID, "url": created.HTMLURL},
	}, nil
}

// ── list_prs ──────────────────────────────────────────────────────────────────

type prItem struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Head      string   `json:"head"`
	Base      string   `json:"base"`
	Author    string   `json:"author"`
	CreatedAt string   `json:"created_at"`
	Labels    []string `json:"labels,omitempty"`
	URL       string   `json:"url"`
}

func (a *GitHubAdapter) listPRs(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github list_prs: %w", err)
	}
	state, _ := params["state"].(string)
	if state == "" {
		state = "open"
	}
	maxResults := 30
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 100 {
		maxResults = v
	}

	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", fmt.Sprintf("%d", maxResults))

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?%s",
		url.PathEscape(owner), url.PathEscape(repo), q.Encode())
	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		CreatedAt string `json:"created_at"`
		HTMLURL   string `json:"html_url"`
		User      struct{ Login string `json:"login"` } `json:"user"`
		Head      struct{ Ref string `json:"ref"` }    `json:"head"`
		Base      struct{ Ref string `json:"ref"` }    `json:"base"`
		Labels    []struct{ Name string `json:"name"` } `json:"labels"`
	}
	if err := apiGET(ctx, client, apiURL, &raw); err != nil {
		return nil, fmt.Errorf("github list_prs: %w", err)
	}

	items := make([]prItem, 0, len(raw))
	for _, r := range raw {
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		items = append(items, prItem{
			Number:    r.Number,
			Title:     format.SanitizeText(r.Title, format.MaxFieldLen),
			State:     r.State,
			Head:      r.Head.Ref,
			Base:      r.Base.Ref,
			Author:    r.User.Login,
			CreatedAt: r.CreatedAt,
			Labels:    labels,
			URL:       r.HTMLURL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d %s PR(s) in %s/%s", len(items), state, owner, repo),
		Data:    items,
	}, nil
}

// ── get_pr ────────────────────────────────────────────────────────────────────

func (a *GitHubAdapter) getPR(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	owner, repo, err := requireOwnerRepo(params)
	if err != nil {
		return nil, fmt.Errorf("github get_pr: %w", err)
	}
	number, ok := paramInt(params, "number")
	if !ok {
		return nil, fmt.Errorf("github get_pr: number is required")
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d",
		url.PathEscape(owner), url.PathEscape(repo), number)
	var raw struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		CreatedAt string `json:"created_at"`
		User      struct{ Login string `json:"login"` }    `json:"user"`
		Head      struct{ Ref string `json:"ref"` }        `json:"head"`
		Base      struct{ Ref string `json:"ref"` }        `json:"base"`
		Merged    bool                                     `json:"merged"`
		Labels    []struct{ Name string `json:"name"` }   `json:"labels"`
	}
	if err := apiGET(ctx, client, apiURL, &raw); err != nil {
		return nil, fmt.Errorf("github get_pr: %w", err)
	}

	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}
	result := map[string]any{
		"number":     raw.Number,
		"title":      format.SanitizeText(raw.Title, format.MaxFieldLen),
		"body":       format.SanitizeText(raw.Body, format.MaxBodyLen),
		"state":      raw.State,
		"merged":     raw.Merged,
		"author":     raw.User.Login,
		"head":       raw.Head.Ref,
		"base":       raw.Base.Ref,
		"labels":     labels,
		"created_at": raw.CreatedAt,
		"url":        raw.HTMLURL,
	}
	return &adapters.Result{
		Summary: format.Summary("PR #%d: %s [%s]", raw.Number, raw.Title, raw.State),
		Data:    result,
	}, nil
}

// ── list_repos ────────────────────────────────────────────────────────────────

type repoItem struct {
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private"`
	Language    string `json:"language,omitempty"`
	Stars       int    `json:"stars"`
	URL         string `json:"url"`
}

func (a *GitHubAdapter) listRepos(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	var apiURL string
	if org, ok := params["org"].(string); ok && org != "" {
		apiURL = fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=50&sort=updated",
			url.PathEscape(org))
	} else {
		apiURL = "https://api.github.com/user/repos?per_page=50&sort=updated&affiliation=owner"
	}

	var raw []struct {
		Name            string `json:"name"`
		FullName        string `json:"full_name"`
		Description     string `json:"description"`
		Private         bool   `json:"private"`
		Language        string `json:"language"`
		StargazersCount int    `json:"stargazers_count"`
		HTMLURL         string `json:"html_url"`
	}
	if err := apiGET(ctx, client, apiURL, &raw); err != nil {
		return nil, fmt.Errorf("github list_repos: %w", err)
	}

	items := make([]repoItem, 0, len(raw))
	for _, r := range raw {
		items = append(items, repoItem{
			Name:        r.Name,
			FullName:    r.FullName,
			Description: format.SanitizeText(r.Description, format.MaxSnippetLen),
			Private:     r.Private,
			Language:    r.Language,
			Stars:       r.StargazersCount,
			URL:         r.HTMLURL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d repository(-ies)", len(items)),
		Data:    items,
	}, nil
}

// ── search_code ───────────────────────────────────────────────────────────────

func (a *GitHubAdapter) searchCode(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("github search_code: query is required")
	}
	if repo, ok := params["repo"].(string); ok && repo != "" {
		query = fmt.Sprintf("%s repo:%s", query, repo)
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("per_page", "20")

	apiURL := "https://api.github.com/search/code?" + q.Encode()
	var resp struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Name       string `json:"name"`
			Path       string `json:"path"`
			HTMLURL    string `json:"html_url"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"items"`
	}
	if err := apiGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("github search_code: %w", err)
	}

	type codeItem struct {
		File    string `json:"file"`
		Path    string `json:"path"`
		Repo    string `json:"repo"`
		URL     string `json:"url"`
	}
	items := make([]codeItem, 0, len(resp.Items))
	for _, r := range resp.Items {
		items = append(items, codeItem{
			File: r.Name,
			Path: r.Path,
			Repo: r.Repository.FullName,
			URL:  r.HTMLURL,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d of %d code result(s) for %q", len(items), resp.TotalCount, query),
		Data:    items,
	}, nil
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

func apiWrite(ctx context.Context, client *http.Client, method, apiURL string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

func requireOwnerRepo(params map[string]any) (owner, repo string, err error) {
	owner, _ = params["owner"].(string)
	repo, _ = params["repo"].(string)
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("owner and repo are required")
	}
	return owner, repo, nil
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
