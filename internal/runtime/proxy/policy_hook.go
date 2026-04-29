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
		if match, task, err := matchPreferredEgressTask(req.Context(), hooks.Store, st.Session.ID, candidateTasks, egressReq); err == nil && match != nil {
			matchedTask = task
			matchedWhy = match.Item.Why
		} else if err != nil {
			return req, goproxy.NewResponse(req, "application/json", http.StatusBadRequest, `{"error":"invalid task egress matcher"}`)
		}

		if oneOff, err := hooks.Store.ConsumeAgentOneOffApproval(req.Context(), st.Session.AgentID, reqFingerprint, time.Now().UTC()); err == nil && oneOff != nil {
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, matchedTask, oneOff.ApprovalID, paramsSafe, req.Method, "allow", "approved", matchedWhy, false, false)
			return req, nil
		}

		if matchedTask != nil {
			s.recordTaskActivity(req.Context(), hooks.Store, st.Session, matchedTask, st.RequestID)
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, matchedTask, nil, paramsSafe, req.Method, "allow", "approved", matchedWhy, false, false)
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
			st.AuditID = s.logAudit(req.Context(), hooks.Store, st, nil, nil, paramsSafe, req.Method, "allow", "observed", "observation mode: request would require runtime approval", true, true)
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

		st.AuditID = s.logAudit(req.Context(), hooks.Store, st, nil, &rec.ID, paramsSafe, req.Method, "review", "pending", "runtime egress request is outside the active task envelope", false, true)
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

func (s *Server) logAudit(ctx context.Context, st store.Store, reqState *RequestState, task *store.Task, approvalID *string, paramsSafe json.RawMessage, method, decision, outcome, reason string, wouldBlock, wouldReview bool) string {
	auditID := uuid.NewString()
	var taskID *string
	var matchedTaskID *string
	if task != nil {
		taskID = &task.ID
		matchedTaskID = &task.ID
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	sessionID := reqState.Session.ID
	agentID := reqState.Session.AgentID
	_ = st.LogAudit(ctx, &store.AuditEntry{
		ID:            auditID,
		UserID:        reqState.Session.UserID,
		AgentID:       &agentID,
		RequestID:     reqState.RequestID,
		TaskID:        taskID,
		SessionID:     &sessionID,
		ApprovalID:    approvalID,
		MatchedTaskID: matchedTaskID,
		Timestamp:     time.Now().UTC(),
		Service:       "runtime.egress",
		Action:        strings.ToLower(method),
		ParamsSafe:    paramsSafe,
		Decision:      decision,
		Outcome:       outcome,
		Reason:        reasonPtr,
		WouldBlock:    wouldBlock,
		WouldReview:   wouldReview,
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

func fingerprintRequest(agentID string, req *http.Request, body []byte) string {
	sum := sha256.Sum256([]byte(agentID + "|" + req.Method + "|" + requestHost(req) + "|" + req.URL.Path + "|" + req.URL.RawQuery + "|" + hex.EncodeToString(body)))
	return hex.EncodeToString(sum[:])
}

func matchPreferredEgressTask(ctx context.Context, st store.Store, sessionID string, tasks []*store.Task, req runtimepolicy.EgressRequest) (*runtimepolicy.EgressMatch, *store.Task, error) {
	if len(tasks) == 0 {
		return nil, nil, nil
	}
	preferred, fallback := partitionTasksByActiveSession(ctx, st, sessionID, tasks)
	if match, task, err := matchEgressTask(preferred, req); err != nil || match != nil {
		return match, task, err
	}
	return matchEgressTask(fallback, req)
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
