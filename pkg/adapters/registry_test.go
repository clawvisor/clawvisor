package adapters_test

import (
	"context"
	"testing"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// stubAdapter is a minimal Adapter implementation for registry order tests.
type stubAdapter struct {
	id      string
	actions []string
}

func (s *stubAdapter) ServiceID() string          { return s.id }
func (s *stubAdapter) SupportedActions() []string { return s.actions }
func (s *stubAdapter) Execute(context.Context, adapters.Request) (*adapters.Result, error) {
	return nil, nil
}
func (s *stubAdapter) OAuthConfig() *oauth2.Config                       { return nil }
func (s *stubAdapter) CredentialFromToken(*oauth2.Token) ([]byte, error) { return nil, nil }
func (s *stubAdapter) ValidateCredential([]byte) error                   { return nil }
func (s *stubAdapter) RequiredScopes() []string                          { return nil }

// TestGetForUser_PerUserOverridesGlobal locks in the fix for the review:
// the global registry must NOT shadow a per-user adapter for the same
// service ID. Before the fix, MCP services registered an empty global
// stub that masked per-user clones holding the discovered tool set,
// causing the gateway preflight to reject every action as "does not exist".
func TestGetForUser_PerUserOverridesGlobal(t *testing.T) {
	reg := adapters.NewRegistry()
	global := &stubAdapter{id: "svc", actions: nil} // empty global stub
	perUser := &stubAdapter{id: "svc", actions: []string{"x", "y", "z"}}

	reg.Register(global)
	reg.RegisterForUser("user-1", perUser)

	got, ok := reg.GetForUser(context.Background(), "svc", "user-1")
	if !ok {
		t.Fatal("GetForUser returned false")
	}
	if len(got.SupportedActions()) != 3 {
		t.Fatalf("expected per-user adapter with 3 actions, got %d (got the global stub instead)", len(got.SupportedActions()))
	}

	// A different user with no per-user registration should still see the global.
	got2, ok2 := reg.GetForUser(context.Background(), "svc", "user-2")
	if !ok2 {
		t.Fatal("GetForUser for unrelated user returned false")
	}
	if len(got2.SupportedActions()) != 0 {
		t.Fatalf("user-2 should see the global stub with 0 actions, got %d", len(got2.SupportedActions()))
	}
}

// TestGetForUser_ResolverHydratesAcrossRestart proves the resolver runs
// when neither per-user cache nor global has a populated adapter. This is
// the post-restart hydration path.
func TestGetForUser_ResolverHydratesAcrossRestart(t *testing.T) {
	reg := adapters.NewRegistry()
	global := &stubAdapter{id: "mcp-svc", actions: nil}
	reg.Register(global)

	// Resolver returns a per-user clone with tools (mimics DB hydration).
	resolverCalls := 0
	reg.SetResolver(func(_ context.Context, serviceID, userID string) (adapters.Adapter, bool) {
		resolverCalls++
		if serviceID == "mcp-svc" && userID == "user-1" {
			return &stubAdapter{id: "mcp-svc", actions: []string{"a", "b"}}, true
		}
		return nil, false
	})

	// First call: resolver runs.
	got, ok := reg.GetForUser(context.Background(), "mcp-svc", "user-1")
	if !ok {
		t.Fatal("first GetForUser returned false")
	}
	if len(got.SupportedActions()) != 2 {
		t.Fatalf("expected resolver to populate 2 actions, got %d", len(got.SupportedActions()))
	}
	if resolverCalls != 1 {
		t.Fatalf("expected 1 resolver call, got %d", resolverCalls)
	}

	// Second call: should hit the per-user cache, not call resolver again.
	got2, ok2 := reg.GetForUser(context.Background(), "mcp-svc", "user-1")
	if !ok2 {
		t.Fatal("second GetForUser returned false")
	}
	if len(got2.SupportedActions()) != 2 {
		t.Fatalf("cached adapter lost actions: got %d", len(got2.SupportedActions()))
	}
	if resolverCalls != 1 {
		t.Fatalf("expected resolver cache hit, got %d total resolver calls", resolverCalls)
	}
}

func TestAllForUser_ListerHydratesUserOnlyAdapters(t *testing.T) {
	reg := adapters.NewRegistry()
	reg.Register(&stubAdapter{id: "builtin", actions: []string{"read"}})

	listerCalls := 0
	reg.SetUserAdapterLister(func(_ context.Context, userID string) ([]adapters.Adapter, bool) {
		listerCalls++
		if userID == "alice" {
			return []adapters.Adapter{
				&stubAdapter{id: "generated", actions: []string{"custom_action"}},
			}, true
		}
		return nil, true
	})

	all := reg.AllForUser(context.Background(), "alice")
	seen := map[string]adapters.Adapter{}
	for _, a := range all {
		seen[a.ServiceID()] = a
	}
	if _, ok := seen["builtin"]; !ok {
		t.Fatal("AllForUser omitted shared built-in adapter")
	}
	generated, ok := seen["generated"]
	if !ok {
		t.Fatal("AllForUser omitted user-only generated adapter")
	}
	if len(generated.SupportedActions()) != 1 {
		t.Fatalf("generated adapter lost actions: got %d", len(generated.SupportedActions()))
	}

	got, ok := reg.GetForUser(context.Background(), "generated", "alice")
	if !ok {
		t.Fatal("listed generated adapter was not cached for GetForUser")
	}
	if got.ServiceID() != "generated" {
		t.Fatalf("cached wrong adapter: got %q", got.ServiceID())
	}

	bobAll := reg.AllForUser(context.Background(), "bob")
	for _, a := range bobAll {
		if a.ServiceID() == "generated" {
			t.Fatal("bob saw alice's generated adapter")
		}
	}
	if listerCalls != 2 {
		t.Fatalf("expected lister to be called for alice and bob, got %d", listerCalls)
	}
}

func TestAllForUser_ListerCannotOverrideGlobalAdapter(t *testing.T) {
	reg := adapters.NewRegistry()
	reg.Register(&stubAdapter{id: "builtin", actions: []string{"shared"}})
	reg.SetUserAdapterLister(func(_ context.Context, userID string) ([]adapters.Adapter, bool) {
		if userID == "alice" {
			return []adapters.Adapter{
				&stubAdapter{id: "builtin", actions: []string{"generated"}},
			}, true
		}
		return nil, true
	})

	all := reg.AllForUser(context.Background(), "alice")
	for _, a := range all {
		if a.ServiceID() == "builtin" {
			actions := a.SupportedActions()
			if len(actions) != 1 || actions[0] != "shared" {
				t.Fatalf("user lister overrode global adapter: actions=%v", actions)
			}
			return
		}
	}
	t.Fatal("AllForUser omitted built-in adapter")
}
