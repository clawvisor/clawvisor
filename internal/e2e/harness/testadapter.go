package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// TestEchoAdapter is a credential-free adapter scenarios use to drive the
// gateway → adapter chain end-to-end. It is registered as service id
// "test.echo". ValidateCredential accepts nil, so the gateway / tasks
// handler treats it as "active" once the harness writes a ServiceMeta row
// for the seeded user. There is no OAuth flow; tests bypass auth.
//
// The adapter has two actions:
//
//	echo        — returns the params back as the result body (no I/O).
//	fetch_url   — GETs params["url"] (a string) using fetchClient and
//	              returns the response body as a string. This lets a
//	              scenario verify the gateway → adapter → upstream chain
//	              by registering an Upstreams.AddJSON for the target host
//	              and asserting the per-host hit counter went up.
type TestEchoAdapter struct {
	mu          sync.Mutex
	fetchClient *http.Client
}

// SetFetchClient configures the http.Client the fetch_url action uses.
// Tests typically set this to a client that routes through the harness
// proxy so adapter-issued requests show up in the same per-host hit
// counter as the agent's direct calls.
func (a *TestEchoAdapter) SetFetchClient(c *http.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetchClient = c
}

func (a *TestEchoAdapter) ServiceID() string { return "test.echo" }

func (a *TestEchoAdapter) SupportedActions() []string {
	return []string{"echo", "fetch_url"}
}

func (a *TestEchoAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "echo":
		// Always include the action name and a non-nil Data so harness
		// assertions don't break when an LLM-generated request body
		// drops or renames the params key.
		params := req.Params
		if params == nil {
			params = map[string]any{}
		}
		body, _ := json.Marshal(params)
		summary := fmt.Sprintf("test.echo:echo received params=%s", string(body))
		return &adapters.Result{
			Summary: summary,
			Data: map[string]any{
				"action":      "echo",
				"params":      params,
				"params_seen": len(params),
				"echo":        summary,
			},
		}, nil
	case "fetch_url":
		raw, ok := req.Params["url"].(string)
		if !ok || raw == "" {
			return nil, fmt.Errorf("fetch_url: params.url is required")
		}
		a.mu.Lock()
		client := a.fetchClient
		a.mu.Unlock()
		if client == nil {
			return nil, fmt.Errorf("fetch_url: harness fetch client not wired")
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, fmt.Errorf("fetch_url: build request: %w", err)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("fetch_url: do: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return &adapters.Result{
			Summary: fmt.Sprintf("fetched %s → %d", raw, resp.StatusCode),
			Data:    map[string]any{"status": resp.StatusCode, "body": string(body)},
		}, nil
	}
	return nil, fmt.Errorf("test.echo: unknown action %q", req.Action)
}

func (a *TestEchoAdapter) OAuthConfig() *oauth2.Config { return nil }

func (a *TestEchoAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("test.echo: OAuth not supported")
}

// ValidateCredential accepts nil — the adapter is credential-free. This
// is what flips serviceActivated() in tasks.go onto the meta-only
// branch (no vault entry needed).
func (a *TestEchoAdapter) ValidateCredential(_ []byte) error { return nil }

func (a *TestEchoAdapter) RequiredScopes() []string { return nil }
