package harness

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// APIHost is the synthetic hostname the harness's clawvisor API mux is
// registered behind. Scenarios point the agent at https://api.clawvisor.test
// to call /api/tasks, /api/gateway/request, etc.
const APIHost = "api.clawvisor.test"

// APISurface bundles the in-process Clawvisor API the harness exposes as a
// routable upstream. It is a real handlers.{Tasks,Approvals,Gateway,Runtime}
// stack — same code paths as production — wrapped in middleware that injects
// the seeded principal directly instead of running JWT/token-hash auth.
type APISurface struct {
	AdapterReg *adapters.Registry
	Echo       *TestEchoAdapter

	tasksHandler     *handlers.TasksHandler
	approvalsHandler *handlers.ApprovalsHandler
	gatewayHandler   *handlers.GatewayHandler

	mux *http.ServeMux
}

// buildAPISurface constructs the handlers + mux. The principal is read from
// the Server at request time so seeding can happen after Start().
func buildAPISurface(s *Server, logger *slog.Logger) *APISurface {
	reg := adapters.NewRegistry()
	echo := &TestEchoAdapter{}
	reg.Register(echo)

	cfg := *s.Config
	hub := events.NewHub()
	assessor := taskrisk.NoopAssessor{}
	verifier := intent.NoopVerifier{}
	extractor := intent.NoopExtractor{}
	baseURL := "http://" + APIHost

	tasksH := handlers.NewTasksHandler(s.Store, s.Vault, reg, nil, cfg, logger, baseURL, hub, assessor)
	approvalsH := handlers.NewApprovalsHandler(s.Store, s.Vault, reg, nil, cfg, assessor, logger, hub)
	gatewayH := handlers.NewGatewayHandler(s.Store, s.Vault, reg, nil, verifier, extractor, cfg, logger, baseURL, hub)

	mux := http.NewServeMux()

	auth := func(h http.HandlerFunc) http.Handler { return s.testAuthMiddleware(h) }

	// Tasks (agent token in prod; harness injects)
	mux.Handle("POST /api/tasks", auth(tasksH.Create))
	mux.Handle("GET /api/tasks/{id}", auth(tasksH.Get))
	mux.Handle("POST /api/tasks/{id}/start", auth(tasksH.Start))
	mux.Handle("POST /api/tasks/{id}/end", auth(tasksH.End))
	mux.Handle("POST /api/tasks/{id}/complete", auth(tasksH.Complete))
	mux.Handle("POST /api/tasks/{id}/expand", auth(tasksH.Expand))

	// Tasks (user JWT in prod)
	mux.Handle("GET /api/tasks", auth(tasksH.List))
	mux.Handle("POST /api/tasks/{id}/approve", auth(tasksH.Approve))
	mux.Handle("POST /api/tasks/{id}/deny", auth(tasksH.Deny))
	mux.Handle("POST /api/tasks/{id}/revoke", auth(tasksH.Revoke))
	mux.Handle("POST /api/tasks/{id}/expand/approve", auth(tasksH.ExpandApprove))
	mux.Handle("POST /api/tasks/{id}/expand/deny", auth(tasksH.ExpandDeny))

	// Gateway (agent token in prod)
	mux.Handle("POST /api/gateway/request", auth(gatewayH.HandleRequest))
	mux.Handle("GET /api/gateway/request/{request_id}", auth(gatewayH.HandleGet))
	mux.Handle("POST /api/gateway/request/{request_id}/execute", auth(gatewayH.HandleExecuteApproved))

	// Approvals (user JWT in prod)
	mux.Handle("GET /api/approvals", auth(approvalsH.List))
	mux.Handle("POST /api/approvals/{request_id}/approve", auth(approvalsH.Approve))
	mux.Handle("POST /api/approvals/{request_id}/deny", auth(approvalsH.Deny))

	return &APISurface{
		AdapterReg:       reg,
		Echo:             echo,
		tasksHandler:     tasksH,
		approvalsHandler: approvalsH,
		gatewayHandler:   gatewayH,
		mux:              mux,
	}
}

// testAuthMiddleware injects the harness's seeded principal directly into
// request context (both User and Agent slots). Returns 401 UNAUTHORIZED if
// the harness has not seeded a principal yet — scenarios must call
// SeedPrincipal first.
func (s *Server) testAuthMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.principalMu.Lock()
		p := s.principal
		s.principalMu.Unlock()
		if p == nil {
			http.Error(w, `{"error":"harness: no principal seeded","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		ctx = context.WithValue(ctx, middleware.UserContextKey, p.User)
		ctx = store.WithAgent(ctx, p.Agent)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ServeHTTP makes APISurface itself a routable handler the upstreams layer
// can mount.
func (a *APISurface) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }
