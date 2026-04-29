package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeUnificationRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := NewStore(db)

	user, err := st.CreateUser(ctx, "runtime@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(30 * time.Minute)
	sessionID := "sess-1"
	requestID := "req-1"
	task := &store.Task{
		UserID:                 user.ID,
		AgentID:                agent.ID,
		Purpose:                "Handle support triage",
		Status:                 "active",
		Lifetime:               "session",
		AuthorizedActions:      []store.TaskAction{{Service: "google.gmail", Action: "list_messages", AutoExecute: true}},
		PlannedCalls:           []store.PlannedCall{{Service: "google.gmail", Action: "list_messages", Reason: "list inbox"}},
		ExpectedTools:          []byte(`[{"tool_name":"fetch_messages","why":"triage inbox"}]`),
		ExpectedEgress:         []byte(`[{"host":"api.example.com","why":"load ticket data"}]`),
		IntentVerificationMode: "strict",
		ExpectedUse:            "Support inbox triage",
		SchemaVersion:          2,
		CreatedAt:              now,
		ApprovedAt:             &now,
		ExpiresAt:              &expiresAt,
		ExpiresInSeconds:       1800,
		RequestCount:           1,
		RiskLevel:              "medium",
		RiskDetails:            []byte(`{"risk":"medium"}`),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	gotTask, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.SchemaVersion != 2 {
		t.Fatalf("SchemaVersion=%d, want 2", gotTask.SchemaVersion)
	}
	if string(gotTask.ExpectedTools) != `[{"tool_name":"fetch_messages","why":"triage inbox"}]` {
		t.Fatalf("ExpectedTools=%s", string(gotTask.ExpectedTools))
	}
	if string(gotTask.ExpectedEgress) != `[{"host":"api.example.com","why":"load ticket data"}]` {
		t.Fatalf("ExpectedEgress=%s", string(gotTask.ExpectedEgress))
	}
	if gotTask.IntentVerificationMode != "strict" {
		t.Fatalf("IntentVerificationMode=%q", gotTask.IntentVerificationMode)
	}
	if gotTask.ExpectedUse != "Support inbox triage" {
		t.Fatalf("ExpectedUse=%q", gotTask.ExpectedUse)
	}

	approvalExpires := now.Add(5 * time.Minute)
	approval := &store.ApprovalRecord{
		Kind:                "task_call_review",
		UserID:              user.ID,
		AgentID:             &agent.ID,
		RequestID:           &requestID,
		TaskID:              &task.ID,
		SessionID:           &sessionID,
		Status:              "pending",
		Surface:             "inline",
		SummaryJSON:         []byte(`{"summary":"review tool call"}`),
		PayloadJSON:         []byte(`{"tool_name":"fetch_messages"}`),
		ResolutionTransport: "release_held_tool_use",
		ExpiresAt:           &approvalExpires,
	}
	if err := st.CreateApprovalRecord(ctx, approval); err != nil {
		t.Fatalf("CreateApprovalRecord: %v", err)
	}
	if _, err := st.GetApprovalRecord(ctx, approval.ID); err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if _, err := st.GetApprovalRecordByRequestID(ctx, requestID); err != nil {
		t.Fatalf("GetApprovalRecordByRequestID: %v", err)
	}
	records, err := st.ListPendingApprovalRecords(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovalRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("ListPendingApprovalRecords len=%d, want 1", len(records))
	}
	if err := st.ResolveApprovalRecord(ctx, approval.ID, "allow_once", "approved", now); err != nil {
		t.Fatalf("ResolveApprovalRecord: %v", err)
	}

	pending := &store.PendingApproval{
		UserID:           user.ID,
		RequestID:        requestID,
		AuditID:          "audit-1",
		ApprovalRecordID: &approval.ID,
		RequestBlob:      []byte(`{"request_id":"req-1"}`),
		Status:           "pending",
		ExpiresAt:        approvalExpires,
	}
	if err := st.SavePendingApproval(ctx, pending); err != nil {
		t.Fatalf("SavePendingApproval: %v", err)
	}
	gotPending, err := st.GetPendingApproval(ctx, requestID)
	if err != nil {
		t.Fatalf("GetPendingApproval: %v", err)
	}
	if gotPending.ApprovalRecordID == nil || *gotPending.ApprovalRecordID != approval.ID {
		t.Fatalf("ApprovalRecordID=%v, want %q", gotPending.ApprovalRecordID, approval.ID)
	}

	audit := &store.AuditEntry{
		ID:                      "audit-runtime",
		UserID:                  user.ID,
		AgentID:                 &agent.ID,
		RequestID:               requestID,
		TaskID:                  &task.ID,
		SessionID:               &sessionID,
		ApprovalID:              &approval.ID,
		LeaseID:                 strPtr("lease-1"),
		ToolUseID:               strPtr("tool-1"),
		MatchedTaskID:           &task.ID,
		LeaseTaskID:             &task.ID,
		Timestamp:               now,
		Service:                 "google.gmail",
		Action:                  "list_messages",
		ParamsSafe:              []byte(`{"max_results":10}`),
		Decision:                "allow",
		Outcome:                 "executed",
		ResolutionConfidence:    strPtr("high"),
		IntentVerdict:           strPtr("aligned"),
		UsedActiveTaskContext:   true,
		UsedLeaseBias:           true,
		UsedConvJudgeResolution: true,
		WouldBlock:              false,
		WouldReview:             true,
		WouldPromptInline:       true,
	}
	if err := st.LogAudit(ctx, audit); err != nil {
		t.Fatalf("LogAudit: %v", err)
	}
	gotAudit, err := st.GetAuditEntry(ctx, audit.ID, user.ID)
	if err != nil {
		t.Fatalf("GetAuditEntry: %v", err)
	}
	if gotAudit.SessionID == nil || *gotAudit.SessionID != sessionID {
		t.Fatalf("SessionID=%v, want %q", gotAudit.SessionID, sessionID)
	}
	if !gotAudit.UsedLeaseBias || !gotAudit.WouldPromptInline {
		t.Fatalf("runtime audit flags not persisted: %+v", gotAudit)
	}

	runtimeSession := &store.RuntimeSession{
		ID:                    sessionID,
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ObservationMode:       true,
		MetadataJSON:          []byte(`{"launcher":"local"}`),
		ExpiresAt:             now.Add(15 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, runtimeSession); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := st.GetRuntimeSession(ctx, runtimeSession.ID); err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	bySecret, err := st.GetRuntimeSessionByProxyBearerSecretHash(ctx, "secret-hash")
	if err != nil {
		t.Fatalf("GetRuntimeSessionByProxyBearerSecretHash: %v", err)
	}
	if bySecret.ID != runtimeSession.ID {
		t.Fatalf("runtime session ID=%q, want %q", bySecret.ID, runtimeSession.ID)
	}
	if err := st.RevokeRuntimeSession(ctx, runtimeSession.ID, now); err != nil {
		t.Fatalf("RevokeRuntimeSession: %v", err)
	}

	runtimeSession2 := &store.RuntimeSession{
		ID:                    "sess-2",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash-2",
		ExpiresAt:             now.Add(20 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, runtimeSession2); err != nil {
		t.Fatalf("CreateRuntimeSession(second): %v", err)
	}
	if err := st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		SessionID:    sessionID,
		UserID:       user.ID,
		AgentID:      agent.ID,
		Provider:     "anthropic",
		EventType:    "runtime.tool_use.held",
		ActionKind:   "tool_use",
		TaskID:       &task.ID,
		Reason:       strPtr("runtime tool call is outside the active task envelope"),
		MetadataJSON: []byte(`{"tool_name":"fetch_messages"}`),
	}); err != nil {
		t.Fatalf("CreateRuntimeEvent: %v", err)
	}
	events, err := st.ListRuntimeEvents(ctx, user.ID, store.RuntimeEventFilter{SessionID: sessionID, Limit: 10})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	if len(events) != 1 || events[0].EventType != "runtime.tool_use.held" {
		t.Fatalf("unexpected runtime events: %+v", events)
	}

	oneOff := &store.OneOffApproval{
		SessionID:          sessionID,
		RequestFingerprint: "fp-1",
		ApprovalID:         &approval.ID,
		ApprovedAt:         now,
		ExpiresAt:          now.Add(2 * time.Minute),
	}
	if err := st.CreateOneOffApproval(ctx, oneOff); err != nil {
		t.Fatalf("CreateOneOffApproval: %v", err)
	}
	consumed, err := st.ConsumeOneOffApproval(ctx, sessionID, "fp-1", now.Add(time.Second))
	if err != nil {
		t.Fatalf("ConsumeOneOffApproval: %v", err)
	}
	if consumed.UsedAt == nil {
		t.Fatal("ConsumeOneOffApproval did not set UsedAt")
	}
	if _, err := st.ConsumeOneOffApproval(ctx, sessionID, "fp-1", now.Add(2*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second ConsumeOneOffApproval err=%v, want ErrNotFound", err)
	}

	agentScopedOneOff := &store.OneOffApproval{
		SessionID:          runtimeSession2.ID,
		RequestFingerprint: "fp-agent",
		ApprovalID:         &approval.ID,
		ApprovedAt:         now,
		ExpiresAt:          now.Add(2 * time.Minute),
	}
	if err := st.CreateOneOffApproval(ctx, agentScopedOneOff); err != nil {
		t.Fatalf("CreateOneOffApproval(agent-scoped): %v", err)
	}
	consumedByAgent, err := st.ConsumeAgentOneOffApproval(ctx, agent.ID, "fp-agent", now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ConsumeAgentOneOffApproval: %v", err)
	}
	if consumedByAgent.SessionID != runtimeSession2.ID {
		t.Fatalf("ConsumeAgentOneOffApproval session=%q, want %q", consumedByAgent.SessionID, runtimeSession2.ID)
	}
	if _, err := st.ConsumeAgentOneOffApproval(ctx, agent.ID, "fp-agent", now.Add(4*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second ConsumeAgentOneOffApproval err=%v, want ErrNotFound", err)
	}

	credAuthExpires := now.Add(10 * time.Minute)
	credAuth := &store.CredentialAuthorization{
		ID:            "cred-auth-1",
		ApprovalID:    &approval.ID,
		UserID:        user.ID,
		AgentID:       agent.ID,
		SessionID:     &runtimeSession2.ID,
		Scope:         "once",
		CredentialRef: "sha256:abc123",
		Service:       "github",
		Host:          "api.github.com",
		HeaderName:    "Authorization",
		Scheme:        "bearer",
		Status:        "active",
		MetadataJSON:  []byte(`{"source":"runtime-review"}`),
		ExpiresAt:     &credAuthExpires,
	}
	if err := st.CreateCredentialAuthorization(ctx, credAuth); err != nil {
		t.Fatalf("CreateCredentialAuthorization: %v", err)
	}
	gotCredAuth, err := st.GetCredentialAuthorization(ctx, credAuth.ID)
	if err != nil {
		t.Fatalf("GetCredentialAuthorization: %v", err)
	}
	if gotCredAuth.Scope != "once" || gotCredAuth.CredentialRef != credAuth.CredentialRef {
		t.Fatalf("unexpected credential authorization: %+v", gotCredAuth)
	}
	consumedCredAuth, err := st.ConsumeMatchingCredentialAuthorization(ctx, store.CredentialAuthorizationMatch{
		UserID:        user.ID,
		AgentID:       agent.ID,
		SessionID:     runtimeSession2.ID,
		CredentialRef: credAuth.CredentialRef,
		Service:       credAuth.Service,
		Host:          credAuth.Host,
		HeaderName:    credAuth.HeaderName,
		Scheme:        credAuth.Scheme,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ConsumeMatchingCredentialAuthorization: %v", err)
	}
	if consumedCredAuth.Status != "used" || consumedCredAuth.UsedAt == nil {
		t.Fatalf("expected once credential authorization to be consumed, got %+v", consumedCredAuth)
	}
	if _, err := st.ConsumeMatchingCredentialAuthorization(ctx, store.CredentialAuthorizationMatch{
		UserID:        user.ID,
		AgentID:       agent.ID,
		SessionID:     runtimeSession2.ID,
		CredentialRef: credAuth.CredentialRef,
		Service:       credAuth.Service,
		Host:          credAuth.Host,
		HeaderName:    credAuth.HeaderName,
		Scheme:        credAuth.Scheme,
	}, now.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second ConsumeMatchingCredentialAuthorization err=%v, want ErrNotFound", err)
	}

	lease := &store.ToolExecutionLease{
		SessionID:    sessionID,
		TaskID:       task.ID,
		ToolUseID:    "tool-1",
		ToolName:     "fetch_messages",
		Status:       "open",
		MetadataJSON: []byte(`{"provider":"anthropic"}`),
		OpenedAt:     now,
		ExpiresAt:    now.Add(5 * time.Minute),
	}
	if err := st.CreateToolExecutionLease(ctx, lease); err != nil {
		t.Fatalf("CreateToolExecutionLease: %v", err)
	}
	openLeases, err := st.ListOpenToolExecutionLeases(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListOpenToolExecutionLeases: %v", err)
	}
	if len(openLeases) != 1 {
		t.Fatalf("ListOpenToolExecutionLeases len=%d, want 1", len(openLeases))
	}
	if err := st.CloseToolExecutionLease(ctx, lease.LeaseID, now.Add(time.Minute), "closed"); err != nil {
		t.Fatalf("CloseToolExecutionLease: %v", err)
	}

	invocation := &store.TaskInvocation{
		TaskID:         task.ID,
		SessionID:      sessionID,
		UserID:         user.ID,
		AgentID:        agent.ID,
		RequestID:      requestID,
		InvocationType: "runtime_proxy",
		Status:         "running",
		MetadataJSON:   []byte(`{"source":"proxy"}`),
		CreatedAt:      now,
	}
	if err := st.CreateTaskInvocation(ctx, invocation); err != nil {
		t.Fatalf("CreateTaskInvocation: %v", err)
	}
	taskCall := &store.TaskCall{
		TaskID:       task.ID,
		InvocationID: invocation.ID,
		RequestID:    requestID,
		SessionID:    sessionID,
		Service:      "google.gmail",
		Action:       "list_messages",
		Outcome:      "executed",
		ApprovalID:   &approval.ID,
		AuditID:      &audit.ID,
		MetadataJSON: []byte(`{"transport":"proxy"}`),
		CreatedAt:    now,
	}
	if err := st.CreateTaskCall(ctx, taskCall); err != nil {
		t.Fatalf("CreateTaskCall: %v", err)
	}

	activeTaskSession := &store.ActiveTaskSession{
		TaskID:       task.ID,
		SessionID:    sessionID,
		UserID:       user.ID,
		AgentID:      agent.ID,
		Status:       "active",
		MetadataJSON: []byte(`{"mode":"runtime"}`),
		StartedAt:    now,
		LastSeenAt:   now,
	}
	if err := st.UpsertActiveTaskSession(ctx, activeTaskSession); err != nil {
		t.Fatalf("UpsertActiveTaskSession: %v", err)
	}
	gotActive, err := st.GetActiveTaskSession(ctx, task.ID, sessionID)
	if err != nil {
		t.Fatalf("GetActiveTaskSession: %v", err)
	}
	if gotActive.Status != "active" {
		t.Fatalf("GetActiveTaskSession status=%q, want active", gotActive.Status)
	}
	if err := st.EndActiveTaskSession(ctx, task.ID, sessionID, now.Add(3*time.Minute), "completed"); err != nil {
		t.Fatalf("EndActiveTaskSession: %v", err)
	}
	if _, err := st.GetActiveTaskSession(ctx, task.ID, sessionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveTaskSession(after end) err=%v, want ErrNotFound", err)
	}
}

func strPtr(v string) *string {
	return &v
}
