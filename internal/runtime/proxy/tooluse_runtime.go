package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/leases"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/internal/runtime/review"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const internalBypassHeader = "X-Clawvisor-Internal-Bypass"

type ToolUseHooks struct {
	Store       store.Store
	Config      *config.Config
	ReviewCache *review.ApprovalCache
	Leases      leases.Service
}

type HeldToolUseApprovalPayload struct {
	SessionID string         `json:"session_id"`
	AgentID   string         `json:"agent_id"`
	TaskID    string         `json:"task_id,omitempty"`
	ToolUseID string         `json:"tool_use_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	Reason    string         `json:"reason,omitempty"`
}

var approvalReplyRE = regexp.MustCompile(`(?i)\b(approve|deny)\s+(cv-[a-z0-9]{12})\b`)
var bareApprovalRE = regexp.MustCompile(`(?i)^\s*(approve|deny)\s*$`)

func (s *Server) InstallToolUseInterceptors(hooks ToolUseHooks) {
	if hooks.Store == nil || hooks.ReviewCache == nil {
		return
	}
	s.installHeldApprovalRelease(hooks)
	s.installToolUseBlocker(hooks)
}

func (s *Server) installHeldApprovalRelease(hooks ToolUseHooks) {
	registry := conversation.DefaultRegistry()
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Header.Get(internalBypassHeader) != "" {
			return req, nil
		}
		parser := registry.Match(req)
		if parser == nil || parser.Name() != conversation.ProviderAnthropic {
			return req, nil
		}
		st := StateOf(ctx)
		if st == nil || st.Session == nil {
			return req, nil
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return req, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))

		s.closeLeasesForToolResults(req.Context(), hooks, st.Session.ID, body)

		held := hooks.ReviewCache.Get(st.Session.ID)
		if held == nil {
			return req, nil
		}

		rec, err := hooks.Store.GetApprovalRecord(req.Context(), held.ApprovalRecordID)
		if err == store.ErrNotFound {
			hooks.ReviewCache.Drop(st.Session.ID)
			return req, nil
		}
		if err != nil {
			return req, nil
		}

		switch rec.Status {
		case "approved":
			if resolved := hooks.ReviewCache.Resolve(st.Session.ID, held.ID); resolved != nil {
				return s.syntheticHeldToolUseResponse(req, st.Session, hooks, resolved, true, "approved via dashboard", body)
			}
		case "denied":
			if resolved := hooks.ReviewCache.Resolve(st.Session.ID, held.ID); resolved != nil {
				return s.syntheticHeldToolUseResponse(req, st.Session, hooks, resolved, false, "denied via dashboard", body)
			}
		}

		if hooks.Config == nil || !hooks.Config.RuntimePolicy.InlineApprovalEnabled {
			return req, nil
		}
		verb, approvalID := parseAnthropicApprovalReply(body)
		if verb == "" {
			return req, nil
		}
		if approvalID == "" {
			approvalID = held.ID
		}
		resolved := hooks.ReviewCache.Resolve(st.Session.ID, approvalID)
		if resolved == nil {
			return req, nil
		}
		now := time.Now().UTC()
		if verb == "approve" {
			_ = hooks.Store.ResolveApprovalRecord(req.Context(), resolved.ApprovalRecordID, "allow_once", "approved", now)
			return s.syntheticHeldToolUseResponse(req, st.Session, hooks, resolved, true, "approved inline by user", body)
		}
		_ = hooks.Store.ResolveApprovalRecord(req.Context(), resolved.ApprovalRecordID, "deny", "denied", now)
		return s.syntheticHeldToolUseResponse(req, st.Session, hooks, resolved, false, "denied inline by user", body)
	})
}

func (s *Server) syntheticHeldToolUseResponse(req *http.Request, session *store.RuntimeSession, hooks ToolUseHooks, held *review.HeldApproval, allow bool, reason string, requestBody []byte) (*http.Request, *http.Response) {
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set(internalBypassHeader, "1")

	var leaseID *string
	usedActiveTaskContext := false
	if allow && held.TaskID != "" {
		lease, err := hooks.Leases.Open(req.Context(), session.ID, held.TaskID, held.ToolUseID, held.ToolName, toolLeaseTTL(hooks.Config))
		if err == nil && lease != nil {
			leaseID = &lease.LeaseID
			if task, taskErr := hooks.Store.GetTask(req.Context(), held.TaskID); taskErr == nil {
				usedActiveTaskContext = usedActiveTaskSelection(req.Context(), hooks.Store, session.ID, task)
				s.recordToolActivity(req.Context(), hooks.Store, session, task, held.ToolUseID, held.ToolName, held.ApprovalRecordID, lease)
			}
		}
	}

	s.logToolUseAudit(req.Context(), hooks.Store, session, held.TaskID, held.ApprovalRecordID, leaseID, held.ToolUseID, held.ToolName, boolToDecision(allow), boolToOutcome(allow), reason, usedActiveTaskContext, false, false, false)

	stream := conversation.AnthropicRequestWantsStream(requestBody)
	var bodyBytes []byte
	contentType := "application/json"
	if allow {
		if stream {
			contentType = "text/event-stream"
			bodyBytes = conversation.SynthAnthropicToolUseSSE("", "", "assistant", held.ToolUseID, held.ToolName, held.ToolInput)
		} else {
			bodyBytes = conversation.SynthAnthropicToolUseJSON("", "", "assistant", held.ToolUseID, held.ToolName, held.ToolInput)
		}
	} else {
		msg := "Approval denied. The requested tool call was not performed."
		if stream {
			contentType = "text/event-stream"
			bodyBytes = conversation.SynthAnthropicTextSSE("", "", "assistant", msg)
		} else {
			bodyBytes = conversation.SynthAnthropicTextJSON("", "", "assistant", msg)
		}
	}

	return req, &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{contentType}, "Cache-Control": []string{"no-cache"}},
		Body:          io.NopCloser(bytes.NewReader(bodyBytes)),
		ContentLength: int64(len(bodyBytes)),
		Request:       req,
	}
}

func (s *Server) installToolUseBlocker(hooks ToolUseHooks) {
	registry := conversation.DefaultResponseRegistry()
	s.goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if resp == nil || ctx == nil || ctx.Req == nil {
			return resp
		}
		if ctx.Req.Header.Get(internalBypassHeader) != "" {
			return resp
		}
		st := StateOf(ctx)
		if st == nil || st.Session == nil {
			return resp
		}
		rewriter := registry.Match(ctx.Req, resp)
		if rewriter == nil || resp.Body == nil {
			return resp
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp
		}

		candidateTasks, _ := loadRuntimeCandidateTasks(ctx.Req.Context(), hooks.Store, st.Session)
		reviewTask := selectReviewTaskContext(ctx.Req.Context(), hooks.Store, st.Session.ID, candidateTasks)
		decisionState := map[string]toolDecisionState{}

		evaluator := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
			input := decodeToolInput(tu.Input)
			match, task, usedActive, err := matchPreferredToolTask(ctx.Req.Context(), hooks.Store, st.Session.ID, candidateTasks, tu.Name, input)
			if err == nil && match != nil && task != nil {
				key := toolDecisionKey(tu)
				decisionState[key] = toolDecisionState{Task: task, UsedActiveTaskContext: usedActive}
				return conversation.ToolUseVerdict{Allowed: true, Reason: match.Item.Why}
			}

			key := toolDecisionKey(tu)
			if st.Session.ObservationMode {
				decisionState[key] = toolDecisionState{
					Task:                  reviewTask,
					UsedActiveTaskContext: reviewTask != nil && usedActiveTaskSelection(ctx.Req.Context(), hooks.Store, st.Session.ID, reviewTask),
					WouldReview:           true,
					WouldPromptInline:     hooks.Config != nil && hooks.Config.RuntimePolicy.InlineApprovalEnabled,
				}
				return conversation.ToolUseVerdict{Allowed: true, Reason: "observation mode: tool use would require runtime approval"}
			}

			rec, held, substitute := s.ensureHeldToolUseApproval(ctx.Req.Context(), hooks, st.Session, reviewTask, tu, input)
			if rec != nil {
				decisionState[key] = toolDecisionState{
					Task:              reviewTask,
					ApprovalID:        &rec.ID,
					WouldReview:       true,
					WouldPromptInline: hooks.Config != nil && hooks.Config.RuntimePolicy.InlineApprovalEnabled,
					Held:              held,
				}
			}
			return conversation.ToolUseVerdict{
				Allowed:        false,
				Reason:         "runtime approval required",
				SubstituteWith: substitute,
			}
		}

		result, err := rewriter.Rewrite(body, resp.Header.Get("Content-Type"), evaluator)
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp
		}

		for _, decision := range result.Decisions {
			key := toolDecisionKey(decision.ToolUse)
			state := decisionState[key]
			if decision.Verdict.Allowed {
				var leaseID *string
				if state.Task != nil {
					lease, err := hooks.Leases.Open(ctx.Req.Context(), st.Session.ID, state.Task.ID, decision.ToolUse.ID, decision.ToolUse.Name, toolLeaseTTL(hooks.Config))
					if err == nil && lease != nil {
						leaseID = &lease.LeaseID
						s.recordToolActivity(ctx.Req.Context(), hooks.Store, st.Session, state.Task, decision.ToolUse.ID, decision.ToolUse.Name, "", lease)
					}
				}
				outcome := "approved"
				reason := decision.Verdict.Reason
				if state.WouldReview {
					outcome = "observed"
					if reason == "" {
						reason = "observation mode: tool use would require runtime approval"
					}
				}
				s.logToolUseAudit(ctx.Req.Context(), hooks.Store, st.Session, taskIDOrEmpty(state.Task), stringOrEmpty(state.ApprovalID), leaseID, decision.ToolUse.ID, decision.ToolUse.Name, "allow", outcome, reason, state.UsedActiveTaskContext, state.WouldReview, state.WouldReview, state.WouldPromptInline)
				continue
			}
			s.logToolUseAudit(ctx.Req.Context(), hooks.Store, st.Session, taskIDOrEmpty(state.Task), stringOrEmpty(state.ApprovalID), nil, decision.ToolUse.ID, decision.ToolUse.Name, "review", "pending", "runtime tool call is outside the active task envelope", state.UsedActiveTaskContext, false, true, state.WouldPromptInline)
		}

		resp.Body = io.NopCloser(bytes.NewReader(result.Body))
		resp.ContentLength = int64(len(result.Body))
		return resp
	})
}

type toolDecisionState struct {
	Task                  *store.Task
	ApprovalID            *string
	Held                  *review.HeldApproval
	UsedActiveTaskContext bool
	WouldReview           bool
	WouldPromptInline     bool
}

func (s *Server) ensureHeldToolUseApproval(ctx context.Context, hooks ToolUseHooks, session *store.RuntimeSession, reviewTask *store.Task, tu conversation.ToolUse, input map[string]any) (*store.ApprovalRecord, *review.HeldApproval, string) {
	requestID := "runtime-tooluse:" + session.ID + ":" + tu.ID
	rec, err := hooks.Store.GetApprovalRecordByRequestID(ctx, requestID)
	if err != nil && err != store.ErrNotFound {
		return nil, nil, "Clawvisor could not create the runtime approval needed for this tool call."
	}

	if err == store.ErrNotFound {
		summaryJSON, _ := json.Marshal(map[string]any{
			"tool_name": tu.Name,
			"reason":    "tool call requires runtime approval",
		})
		payloadJSON, _ := json.Marshal(HeldToolUseApprovalPayload{
			SessionID: session.ID,
			AgentID:   session.AgentID,
			TaskID:    taskIDOrEmpty(reviewTask),
			ToolUseID: tu.ID,
			ToolName:  tu.Name,
			ToolInput: input,
			Reason:    "tool call requires runtime approval",
		})
		rec = &store.ApprovalRecord{
			ID:                  uuid.NewString(),
			Kind:                "task_call_review",
			UserID:              session.UserID,
			AgentID:             &session.AgentID,
			RequestID:           &requestID,
			TaskID:              taskIDPtr(reviewTask),
			SessionID:           &session.ID,
			Status:              "pending",
			Surface:             approvalSurface(hooks.Config),
			SummaryJSON:         summaryJSON,
			PayloadJSON:         payloadJSON,
			ResolutionTransport: "release_held_tool_use",
		}
		if createErr := hooks.Store.CreateApprovalRecord(ctx, rec); createErr != nil {
			return nil, nil, "Clawvisor could not create the runtime approval needed for this tool call."
		}
	}

	held, ok := hooks.ReviewCache.Hold(session.ID, rec.ID, taskIDOrEmpty(reviewTask), tu.ID, tu.Name, input, "tool call requires runtime approval")
	if ok {
		return rec, held, renderHeldToolUsePrompt(held, hooks.Config)
	}
	existing := hooks.ReviewCache.Get(session.ID)
	return rec, existing, renderExistingHeldPrompt(existing, hooks.Config)
}

func loadRuntimeCandidateTasks(ctx context.Context, st store.Store, session *store.RuntimeSession) ([]*store.Task, error) {
	tasks, _, err := st.ListTasks(ctx, session.UserID, store.TaskFilter{ActiveOnly: true})
	if err != nil {
		return nil, err
	}
	out := make([]*store.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.Status == "active" && task.AgentID == session.AgentID {
			out = append(out, task)
		}
	}
	return out, nil
}

func matchPreferredToolTask(ctx context.Context, st store.Store, sessionID string, tasks []*store.Task, toolName string, input map[string]any) (*runtimepolicy.ToolMatch, *store.Task, bool, error) {
	if len(tasks) == 0 {
		return nil, nil, false, nil
	}
	preferred, fallback := partitionTasksByActiveSession(ctx, st, sessionID, tasks)
	if match, task, err := matchToolTask(preferred, toolName, input); err != nil || match != nil {
		return match, task, match != nil, err
	}
	match, task, err := matchToolTask(fallback, toolName, input)
	return match, task, false, err
}

func matchToolTask(tasks []*store.Task, toolName string, input map[string]any) (*runtimepolicy.ToolMatch, *store.Task, error) {
	if len(tasks) == 0 {
		return nil, nil, nil
	}
	match, err := runtimepolicy.MatchToolCall(tasks, toolName, input)
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

func selectReviewTaskContext(ctx context.Context, st store.Store, sessionID string, tasks []*store.Task) *store.Task {
	preferred, _ := partitionTasksByActiveSession(ctx, st, sessionID, tasks)
	if len(preferred) == 1 {
		return preferred[0]
	}
	if len(tasks) == 1 {
		return tasks[0]
	}
	return nil
}

func usedActiveTaskSelection(ctx context.Context, st store.Store, sessionID string, task *store.Task) bool {
	if task == nil {
		return false
	}
	_, err := st.GetActiveTaskSession(ctx, task.ID, sessionID)
	return err == nil
}

func decodeToolInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func renderHeldToolUsePrompt(held *review.HeldApproval, cfg *config.Config) string {
	if held == nil {
		return "Tool use requires runtime approval in Clawvisor before it can run."
	}
	if cfg != nil && cfg.RuntimePolicy.InlineApprovalEnabled {
		return "Clawvisor paused a tool call pending approval.\n\nReply `approve " + held.ID + "` to release it, reply `deny " + held.ID + "` to reject it, or approve it from the Clawvisor dashboard."
	}
	return "Clawvisor paused a tool call pending approval in the dashboard.\n\nApproval ID: " + held.ID
}

func renderExistingHeldPrompt(held *review.HeldApproval, cfg *config.Config) string {
	if held == nil {
		return "Clawvisor already has a pending runtime approval for this session."
	}
	return renderHeldToolUsePrompt(held, cfg)
}

func approvalSurface(cfg *config.Config) string {
	if cfg != nil && cfg.RuntimePolicy.InlineApprovalEnabled {
		return "inline"
	}
	return "dashboard"
}

func toolDecisionKey(tu conversation.ToolUse) string {
	if tu.ID != "" {
		return tu.ID
	}
	return tu.Name + ":" + strconv.Itoa(tu.Index)
}

func parseAnthropicApprovalReply(body []byte) (verb, id string) {
	var req anthropicApprovalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		text := extractAnthropicUserText(req.Messages[i].Content)
		if match := approvalReplyRE.FindStringSubmatch(text); match != nil {
			return strings.ToLower(match[1]), strings.ToLower(match[2])
		}
		if match := bareApprovalRE.FindStringSubmatch(text); match != nil {
			return strings.ToLower(match[1]), ""
		}
		return "", ""
	}
	return "", ""
}

type anthropicApprovalRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func extractAnthropicUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			out = append(out, block.Text)
		}
	}
	return strings.Join(out, "\n")
}

func (s *Server) closeLeasesForToolResults(ctx context.Context, hooks ToolUseHooks, sessionID string, body []byte) {
	toolResultIDs := conversation.AnthropicToolResultIDsFromRequest(body)
	if len(toolResultIDs) == 0 {
		return
	}
	leasesForSession, err := hooks.Store.ListOpenToolExecutionLeases(ctx, sessionID)
	if err != nil {
		return
	}
	for _, lease := range leasesForSession {
		for _, toolUseID := range toolResultIDs {
			if lease.ToolUseID == toolUseID {
				_ = hooks.Leases.Close(ctx, lease.LeaseID)
				break
			}
		}
	}
}

func (s *Server) recordToolActivity(ctx context.Context, st store.Store, session *store.RuntimeSession, task *store.Task, toolUseID, toolName, approvalRecordID string, lease *store.ToolExecutionLease) {
	if session == nil || task == nil {
		return
	}
	now := time.Now().UTC()
	metadata, _ := json.Marshal(map[string]any{"tool_use_id": toolUseID})
	_ = st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:       task.ID,
		SessionID:    session.ID,
		UserID:       session.UserID,
		AgentID:      session.AgentID,
		MetadataJSON: metadata,
		StartedAt:    now,
		LastSeenAt:   now,
		Status:       "active",
	})
	call := &store.TaskCall{
		TaskID:       task.ID,
		RequestID:    session.ID + ":" + toolUseID,
		SessionID:    session.ID,
		Service:      "runtime.tool_use",
		Action:       toolName,
		Outcome:      "allowed",
		CreatedAt:    now,
		MetadataJSON: metadata,
	}
	if approvalRecordID != "" {
		call.ApprovalID = &approvalRecordID
	}
	if lease != nil {
		call.InvocationID = lease.LeaseID
	}
	_ = st.CreateTaskCall(ctx, call)
}

func (s *Server) logToolUseAudit(ctx context.Context, st store.Store, session *store.RuntimeSession, taskID, approvalID string, leaseID *string, toolUseID, toolName, decision, outcome, reason string, usedActiveTaskContext, wouldBlock, wouldReview, wouldPromptInline bool) {
	if session == nil {
		return
	}
	var taskIDPtr *string
	if taskID != "" {
		taskIDPtr = &taskID
	}
	var approvalIDPtr *string
	if approvalID != "" {
		approvalIDPtr = &approvalID
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	sessionID := session.ID
	agentID := session.AgentID
	_ = st.LogAudit(ctx, &store.AuditEntry{
		ID:                    uuid.NewString(),
		UserID:                session.UserID,
		AgentID:               &agentID,
		RequestID:             session.ID + ":" + toolUseID,
		TaskID:                taskIDPtr,
		SessionID:             &sessionID,
		ApprovalID:            approvalIDPtr,
		LeaseID:               leaseID,
		ToolUseID:             &toolUseID,
		MatchedTaskID:         taskIDPtr,
		Timestamp:             time.Now().UTC(),
		Service:               "runtime.tool_use",
		Action:                toolName,
		Decision:              decision,
		Outcome:               outcome,
		Reason:                reasonPtr,
		UsedActiveTaskContext: usedActiveTaskContext,
		WouldBlock:            wouldBlock,
		WouldReview:           wouldReview,
		WouldPromptInline:     wouldPromptInline,
	})
}

func toolLeaseTTL(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.RuntimePolicy.ToolLeaseTimeoutSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(cfg.RuntimePolicy.ToolLeaseTimeoutSeconds) * time.Second
}

func taskIDOrEmpty(task *store.Task) string {
	if task == nil {
		return ""
	}
	return task.ID
}

func taskIDPtr(task *store.Task) *string {
	if task == nil {
		return nil
	}
	return &task.ID
}

func stringOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func boolToDecision(allow bool) string {
	if allow {
		return "allow"
	}
	return "block"
}

func boolToOutcome(allow bool) string {
	if allow {
		return "approved"
	}
	return "denied"
}
