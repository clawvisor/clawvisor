package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// captureCtx is a tiny handler that records what RequireUserOrAgent attached
// to the context, so the table-driven test can assert on the resolved identity
// without juggling channels or pointers.
type captureCtx struct {
	User  *store.User
	Agent *store.Agent
}

func (c *captureCtx) handler(w http.ResponseWriter, r *http.Request) {
	c.User = UserFromContext(r.Context())
	c.Agent = AgentFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func TestRequireUserOrAgent_AgentTokenInAuthorization(t *testing.T) {
	st, agent, raw := newSeededAgent(t)
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if cap.Agent == nil || cap.Agent.ID != agent.ID {
		t.Fatalf("agent not attached: got %+v, want id=%s", cap.Agent, agent.ID)
	}
	if cap.User == nil || cap.User.ID != agent.UserID {
		t.Fatalf("owning user not attached: got %+v, want user_id=%s", cap.User, agent.UserID)
	}
}

func TestRequireUserOrAgent_AgentTokenInClawvisorHeader(t *testing.T) {
	st, agent, raw := newSeededAgent(t)
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	req.Header.Set("X-Clawvisor-Agent-Token", raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if cap.Agent == nil || cap.Agent.ID != agent.ID {
		t.Fatalf("agent not attached: got %+v", cap.Agent)
	}
	if cap.User == nil || cap.User.ID != agent.UserID {
		t.Fatalf("owning user not attached: got %+v", cap.User)
	}
}

func TestRequireUserOrAgent_UserJWTStillWorks(t *testing.T) {
	st, agent, _ := newSeededAgent(t) // creates a user too via newSeededAgent
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	user, err := st.GetUserByID(t.Context(), agent.UserID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	jwt, err := jwtSvc.GenerateAccessToken(user.ID, user.Email, time.Hour)
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if cap.User == nil || cap.User.ID != user.ID {
		t.Fatalf("user not attached: got %+v, want id=%s", cap.User, user.ID)
	}
	if cap.Agent != nil {
		t.Fatalf("agent should not be attached on user-JWT path: got %+v", cap.Agent)
	}
}

func TestRequireUserOrAgent_RejectsInvalidAgentToken(t *testing.T) {
	st, _, _ := newSeededAgent(t)
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	req.Header.Set("Authorization", "Bearer cvis_does-not-exist")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
	if cap.User != nil || cap.Agent != nil {
		t.Fatalf("nothing should be attached on rejection: user=%+v agent=%+v", cap.User, cap.Agent)
	}
}

func TestRequireUserOrAgent_RejectsExpiredAgentToken(t *testing.T) {
	st, _, raw := newExpiredSeededAgent(t)
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestRequireUserOrAgent_RejectsMissingAuth(t *testing.T) {
	st, _, _ := newSeededAgent(t)
	jwtSvc, err := auth.NewJWTService("test-secret-test-secret-test-secret-12345")
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}

	cap := &captureCtx{}
	h := RequireUserOrAgent(jwtSvc, st)(http.HandlerFunc(cap.handler))

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}
