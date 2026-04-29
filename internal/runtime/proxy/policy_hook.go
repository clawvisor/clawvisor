package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type RuntimeApprovalPayload struct {
	SessionID          string         `json:"session_id"`
	AgentID            string         `json:"agent_id"`
	RequestFingerprint string         `json:"request_fingerprint"`
	Method             string         `json:"method"`
	Host               string         `json:"host"`
	Path               string         `json:"path"`
	Query              map[string]any `json:"query,omitempty"`
	Body               map[string]any `json:"body,omitempty"`
	Headers            map[string]any `json:"headers,omitempty"`
}

type PolicyHooks struct {
	Store  store.Store
	Config *config.Config
	Logger *slog.Logger
}

func (s *Server) InstallEgressPolicy(hooks PolicyHooks) {
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := EnsureState(ctx)
		if st.Session == nil {
			return req, nil
		}

		bodyBytes, bodyShape, err := readJSONBody(req)
		if err != nil {
			return req, goproxy.NewResponse(req, "application/json", http.StatusBadRequest, `{"error":"invalid JSON request body"}`)
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))

		host := requestHost(req)
		if isHarnessAllowlisted(hooks.Config, host) {
			return req, nil
		}
		reqFingerprint := fingerprintRequest(st.Session.AgentID, req, bodyBytes)
		approvalRequestID := st.Session.ID + ":" + reqFingerprint

		paramsSafe, _ := json.Marshal(map[string]any{
			"host":    host,
			"path":    req.URL.Path,
			"query":   req.URL.Query(),
			"headers": flattenHeaders(req.Header),
		})

		queryShape := urlValuesToMap(req.URL.Query())
		headerShape := flattenHeaders(req.Header)
		headerShapeAny := mapStringToAny(headerShape)
		egressReq := runtimepolicy.EgressRequest{
			Host:    host,
			Method:  req.Method,
			Path:    req.URL.Path,
			Query:   queryShape,
			Body:    bodyShape,
			Headers: headerShape,
		}

		var matchedTask *store.Task
		var matchedWhy string
		var leaseID *string
		var leaseTaskID *string
		var usedActiveTaskContext bool
		var usedLeaseBias bool
		tasks, _, err := hooks.Store.ListTasks(req.Context(), st.Session.UserID, store.TaskFilter{ActiveOnly: true})
		if err != nil {
			return req, goproxy.NewResponse(req, "application/json", http.StatusInternalServerError, `{"error":"could not load tasks"}`)
		}
		var candidateTasks []*store.Task
		for _, task := range tasks {
			if task.Status != "active" || task.AgentID != st.Session.AgentID {
				continue
			}
			candidateTasks = append(candidateTasks, task)
		}
		if matchCtx, err := matchAttributedEgressTask(req.Context(), hooks.Store, st.Session.ID, candidateTasks, egressReq); err == nil && matchCtx != nil && matchCtx.Match != nil {
			matchedTask = matchCtx.Task
			matchedWhy = matchCtx.Match.Item.Why
			leaseID = matchCtx.LeaseID
			leaseTaskID = matchCtx.LeaseTaskID
			usedActiveTaskContext = matchCtx.UsedActiveTaskContext
			usedLeaseBias = matchCtx.UsedLeaseBias
		} else if err != nil {
			return req, goproxy.NewResponse(req, "application/json", http.StatusBadRequest, `{"error":"invalid task egress matcher"}`)
		}

		if oneOff, err := hooks.Store.ConsumeAgentOneOffApproval(req.Context(), st.Session.AgentID, reqFingerprint, time.Now().UTC()); err == nil && oneOff != nil {
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, runtimeAuditOptions{
				MatchedTask:           matchedTask,
				ApprovalID:            oneOff.ApprovalID,
				LeaseID:               leaseID,
				LeaseTaskID:           leaseTaskID,
				UsedActiveTaskContext: usedActiveTaskContext,
				UsedLeaseBias:         usedLeaseBias,
			}, paramsSafe, req.Method, "allow", "approved", matchedWhy)
			return req, nil
		}

		if matchedTask != nil {
			s.recordTaskActivity(req.Context(), hooks.Store, st.Session, matchedTask, st.RequestID)
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, runtimeAuditOptions{
				MatchedTask:           matchedTask,
				LeaseID:               leaseID,
				LeaseTaskID:           leaseTaskID,
				UsedActiveTaskContext: usedActiveTaskContext,
				UsedLeaseBias:         usedLeaseBias,
			}, paramsSafe, req.Method, "allow", "approved", matchedWhy)
			return req, nil
		}

		payload := RuntimeApprovalPayload{
			SessionID:          st.Session.ID,
			AgentID:            st.Session.AgentID,
			RequestFingerprint: reqFingerprint,
			Method:             req.Method,
			Host:               host,
			Path:               req.URL.Path,
			Query:              queryShape,
			Body:               bodyShape,
			Headers:            headerShapeAny,
		}
		payloadJSON, _ := json.Marshal(payload)
		summaryJSON, _ := json.Marshal(map[string]any{
			"method": req.Method,
			"host":   host,
			"path":   req.URL.Path,
		})

		if st.Session.ObservationMode {
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, runtimeAuditOptions{
				WouldBlock:  true,
				WouldReview: true,
			}, paramsSafe, req.Method, "allow", "observed", "observation mode: request would require runtime approval")
			return req, nil
		}

		rec, err := hooks.Store.GetApprovalRecordByRequestID(req.Context(), approvalRequestID)
		if err != nil && err != store.ErrNotFound {
			return req, goproxy.NewResponse(req, "application/json", http.StatusInternalServerError, `{"error":"could not load runtime approval"}`)
		}
		if err == store.ErrNotFound {
			rec = &store.ApprovalRecord{
				ID:                  uuid.NewString(),
				Kind:                "request_once",
				UserID:              st.Session.UserID,
				AgentID:             &st.Session.AgentID,
				RequestID:           &approvalRequestID,
				SessionID:           &st.Session.ID,
				Status:              "pending",
				Surface:             "dashboard",
				SummaryJSON:         json.RawMessage(summaryJSON),
				PayloadJSON:         json.RawMessage(payloadJSON),
				ResolutionTransport: "consume_one_off_retry",
			}
			if err := hooks.Store.CreateApprovalRecord(req.Context(), rec); err != nil {
				return req, goproxy.NewResponse(req, "application/json", http.StatusInternalServerError, `{"error":"could not create runtime approval"}`)
			}
		}

		st.AuditID = s.logAudit(req.Context(), hooks.Store, st, runtimeAuditOptions{
			ApprovalID:        &rec.ID,
			WouldReview:       true,
			WouldBlock:        false,
			WouldPromptInline: false,
		}, paramsSafe, req.Method, "review", "pending", "runtime egress request is outside the active task envelope")
		st.SkipAuditOutcomeUpdate = true

		respBody, _ := json.Marshal(map[string]any{
			"error":               "runtime approval required",
			"code":                "RUNTIME_APPROVAL_REQUIRED",
			"approval_id":         rec.ID,
			"request_fingerprint": reqFingerprint,
		})
		return req, goproxy.NewResponse(req, "application/json", http.StatusForbidden, string(respBody))
	})

	s.goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		st := StateOf(ctx)
		if st == nil || st.AuditID == "" {
			return resp
		}
		if st.SkipAuditOutcomeUpdate {
			return resp
		}
		st.StatusLogged.Do(func() {
			outcome := "executed"
			errMsg := ""
			if resp != nil && resp.StatusCode >= 400 {
				outcome = "http_error"
				errMsg = resp.Status
			}
			_ = hooks.Store.UpdateAuditOutcome(context.Background(), st.AuditID, outcome, errMsg, int(time.Since(st.StartedAt).Milliseconds()))
		})
		return resp
	})
}

type runtimeAuditOptions struct {
	MatchedTask           *store.Task
	ApprovalID            *string
	LeaseID               *string
	ToolUseID             *string
	LeaseTaskID           *string
	UsedActiveTaskContext bool
	UsedLeaseBias         bool
	WouldBlock            bool
	WouldReview           bool
	WouldPromptInline     bool
}

func (s *Server) logAudit(ctx context.Context, st store.Store, reqState *RequestState, opts runtimeAuditOptions, paramsSafe json.RawMessage, method, decision, outcome, reason string) string {
	auditID := uuid.NewString()
	var taskID *string
	var matchedTaskID *string
	if opts.MatchedTask != nil {
		taskID = &opts.MatchedTask.ID
		matchedTaskID = &opts.MatchedTask.ID
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	sessionID := reqState.Session.ID
	agentID := reqState.Session.AgentID
	_ = st.LogAudit(ctx, &store.AuditEntry{
		ID:                    auditID,
		UserID:                reqState.Session.UserID,
		AgentID:               &agentID,
		RequestID:             reqState.RequestID,
		TaskID:                taskID,
		SessionID:             &sessionID,
		ApprovalID:            opts.ApprovalID,
		LeaseID:               opts.LeaseID,
		ToolUseID:             opts.ToolUseID,
		MatchedTaskID:         matchedTaskID,
		LeaseTaskID:           opts.LeaseTaskID,
		Timestamp:             time.Now().UTC(),
		Service:               "runtime.egress",
		Action:                strings.ToLower(method),
		ParamsSafe:            paramsSafe,
		Decision:              decision,
		Outcome:               outcome,
		Reason:                reasonPtr,
		UsedActiveTaskContext: opts.UsedActiveTaskContext,
		UsedLeaseBias:         opts.UsedLeaseBias,
		WouldBlock:            opts.WouldBlock,
		WouldReview:           opts.WouldReview,
		WouldPromptInline:     opts.WouldPromptInline,
	})
	return auditID
}

func (s *Server) recordTaskActivity(ctx context.Context, st store.Store, session *store.RuntimeSession, task *store.Task, requestID string) {
	if session == nil || task == nil {
		return
	}
	now := time.Now().UTC()
	activeStartedAt := now
	if existing, err := st.GetActiveTaskSession(ctx, task.ID, session.ID); err == nil && existing != nil {
		activeStartedAt = existing.StartedAt
	} else if err == store.ErrNotFound {
		inv := &store.TaskInvocation{
			TaskID:         task.ID,
			SessionID:      session.ID,
			UserID:         session.UserID,
			AgentID:        session.AgentID,
			RequestID:      requestID,
			InvocationType: "runtime_proxy",
			Status:         "active",
			CreatedAt:      now,
		}
		if err := st.CreateTaskInvocation(ctx, inv); err == nil {
			activeStartedAt = inv.CreatedAt
		}
	}
	metadata, _ := json.Marshal(map[string]any{"request_id": requestID})
	_ = st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:       task.ID,
		SessionID:    session.ID,
		UserID:       session.UserID,
		AgentID:      session.AgentID,
		MetadataJSON: metadata,
		StartedAt:    activeStartedAt,
		LastSeenAt:   now,
		Status:       "active",
	})
	_ = st.CreateTaskCall(ctx, &store.TaskCall{
		TaskID:    task.ID,
		RequestID: requestID,
		SessionID: session.ID,
		Service:   "runtime.egress",
		Action:    "http",
		Outcome:   "allowed",
		CreatedAt: now,
	})
}

func readJSONBody(req *http.Request) ([]byte, map[string]any, error) {
	if req.Body == nil {
		return nil, nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 {
		return body, nil, nil
	}
	var asMap map[string]any
	if strings.Contains(strings.ToLower(req.Header.Get("Content-Type")), "application/json") {
		if err := json.Unmarshal(body, &asMap); err != nil {
			return nil, nil, err
		}
	}
	return body, asMap, nil
}

func requestHost(req *http.Request) string {
	if req.URL != nil && req.URL.Host != "" {
		return strings.ToLower(req.URL.Hostname())
	}
	return strings.ToLower(req.Host)
}

func isHarnessAllowlisted(cfg *config.Config, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if cfg != nil {
		if endpointHost := normalizedEndpointHost(cfg.LLM.Endpoint); endpointHost != "" && host == endpointHost {
			return true
		}
		for _, allowed := range cfg.RuntimePolicy.HarnessAllowlist {
			allowed = strings.ToLower(strings.TrimSpace(allowed))
			if allowed == "" {
				continue
			}
			if host == allowed || strings.HasSuffix(host, "."+allowed) {
				return true
			}
		}
	}
	switch host {
	case "api.anthropic.com", "api.openai.com", "chatgpt.com":
		return true
	}
	return false
}

func normalizedEndpointHost(endpoint string) string {
	if strings.TrimSpace(endpoint) == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func fingerprintRequest(agentID string, req *http.Request, body []byte) string {
	sum := sha256.Sum256([]byte(agentID + "|" + req.Method + "|" + requestHost(req) + "|" + req.URL.Path + "|" + req.URL.RawQuery + "|" + hex.EncodeToString(body)))
	return hex.EncodeToString(sum[:])
}

type attributedEgressMatch struct {
	Match                 *runtimepolicy.EgressMatch
	Task                  *store.Task
	LeaseID               *string
	LeaseTaskID           *string
	UsedActiveTaskContext bool
	UsedLeaseBias         bool
}

func matchAttributedEgressTask(ctx context.Context, st store.Store, sessionID string, tasks []*store.Task, req runtimepolicy.EgressRequest) (*attributedEgressMatch, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	activePreferred, activeFallback := partitionTasksByActiveSession(ctx, st, sessionID, tasks)
	ordered, leaseID, leaseTaskID, usedLease := reorderTasksByOpenLease(ctx, st, sessionID, activePreferred, activeFallback)
	match, task, err := matchEgressTask(ordered, req)
	if err != nil || match == nil {
		return nil, err
	}
	usedActive := false
	if task != nil {
		usedActive = containsTask(activePreferred, task.ID)
	}
	return &attributedEgressMatch{
		Match:                 match,
		Task:                  task,
		LeaseID:               leaseID,
		LeaseTaskID:           leaseTaskID,
		UsedActiveTaskContext: usedActive,
		UsedLeaseBias:         usedLease && task != nil && leaseTaskID != nil && *leaseTaskID == task.ID,
	}, nil
}

func partitionTasksByActiveSession(ctx context.Context, st store.Store, sessionID string, tasks []*store.Task) ([]*store.Task, []*store.Task) {
	preferred := make([]*store.Task, 0, len(tasks))
	fallback := make([]*store.Task, 0, len(tasks))
	for _, task := range tasks {
		if _, err := st.GetActiveTaskSession(ctx, task.ID, sessionID); err == nil {
			preferred = append(preferred, task)
			continue
		}
		fallback = append(fallback, task)
	}
	return preferred, fallback
}

func reorderTasksByOpenLease(ctx context.Context, st store.Store, sessionID string, preferred []*store.Task, fallback []*store.Task) ([]*store.Task, *string, *string, bool) {
	leasesForSession, err := st.ListOpenToolExecutionLeases(ctx, sessionID)
	if err != nil || len(leasesForSession) == 0 {
		return append(append([]*store.Task{}, preferred...), fallback...), nil, nil, false
	}
	var leaseID *string
	if leasesForSession[0] != nil {
		leaseID = &leasesForSession[0].LeaseID
	}
	var leaseTaskID *string
	if leasesForSession[0] != nil && leasesForSession[0].TaskID != "" {
		leaseTaskID = &leasesForSession[0].TaskID
	}
	leaseTaskSet := map[string]struct{}{}
	for _, lease := range leasesForSession {
		if lease == nil || lease.TaskID == "" {
			continue
		}
		leaseTaskSet[lease.TaskID] = struct{}{}
	}
	ordered := make([]*store.Task, 0, len(preferred)+len(fallback))
	seen := map[string]struct{}{}
	appendOrdered := func(tasks []*store.Task, preferLease bool) {
		for _, task := range tasks {
			if task == nil {
				continue
			}
			_, leaseMatch := leaseTaskSet[task.ID]
			if preferLease != leaseMatch {
				continue
			}
			if _, ok := seen[task.ID]; ok {
				continue
			}
			seen[task.ID] = struct{}{}
			ordered = append(ordered, task)
		}
	}
	appendOrdered(preferred, true)
	appendOrdered(fallback, true)
	appendOrdered(preferred, false)
	appendOrdered(fallback, false)
	return ordered, leaseID, leaseTaskID, true
}

func matchEgressTask(tasks []*store.Task, req runtimepolicy.EgressRequest) (*runtimepolicy.EgressMatch, *store.Task, error) {
	if len(tasks) == 0 {
		return nil, nil, nil
	}
	match, err := runtimepolicy.MatchEgressRequest(tasks, req)
	if err != nil || match == nil {
		return match, nil, err
	}
	for _, task := range tasks {
		if task.ID == match.TaskID {
			return match, task, nil
		}
	}
	return nil, nil, nil
}

func containsTask(tasks []*store.Task, taskID string) bool {
	for _, task := range tasks {
		if task != nil && task.ID == taskID {
			return true
		}
	}
	return false
}

func flattenHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	out := make(map[string]string, len(header))
	for k, vals := range header {
		if len(vals) == 0 {
			continue
		}
		out[strings.ToLower(k)] = vals[0]
	}
	return out
}

func mapStringToAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func urlValuesToMap(values url.Values) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for k, vals := range values {
		if len(vals) == 1 {
			out[k] = vals[0]
			continue
		}
		items := make([]any, len(vals))
		for i, v := range vals {
			items[i] = v
		}
		out[k] = items
	}
	return out
}
