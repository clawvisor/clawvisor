package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Principal is a seeded user/agent pair plus a runtime session the responder
// uses. The harness reuses the runtime proxy's HashProxyBearerSecret so the
// session is authenticatable end-to-end.
type Principal struct {
	User    *store.User
	Agent   *store.Agent
	Session *store.RuntimeSession
}

// SeedPrincipal creates a user, agent, and runtime session in the harness
// store. Pass an empty agentName to default it. The harness records the
// principal so the API mux's test middleware can inject it into context
// for subsequent in-process requests.
//
// SeedPrincipal also activates the test.echo adapter for the user (a
// ServiceMeta row) so scenarios that POST /api/tasks with a service of
// "test.echo" can pass the activation check without an OAuth dance.
func (s *Server) SeedPrincipal(ctx context.Context, agentName string) (*Principal, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("harness: server not started")
	}
	if agentName == "" {
		agentName = "harness-agent"
	}
	user, err := s.Store.CreateUser(ctx, fmt.Sprintf("%s@harness.example", agentName), "harness-hash", "")
	if err != nil {
		return nil, fmt.Errorf("harness: create user: %w", err)
	}
	agent, err := s.Store.CreateAgent(ctx, user.ID, agentName, "harness-token-hash")
	if err != nil {
		return nil, fmt.Errorf("harness: create agent: %w", err)
	}
	// "default" — the canonical alias for an unspecified-account request.
	// parseServiceAlias("test.echo") returns alias="default" so the lookup
	// must match here.
	if err := s.Store.UpsertServiceMeta(ctx, user.ID, "test.echo", "default", time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("harness: activate test.echo: %w", err)
	}
	p := &Principal{User: user, Agent: agent}
	s.principalMu.Lock()
	s.principal = p
	s.principalMu.Unlock()
	return p, nil
}
