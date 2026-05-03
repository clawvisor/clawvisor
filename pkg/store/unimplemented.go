package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is returned by UnimplementedStore methods so external
// implementers that embed it can detect calls to methods they have not yet
// overridden. Callers should not rely on this sentinel for control flow on
// real backends; it exists purely to support partial implementations.
var ErrNotImplemented = errors.New("store: method not implemented")

// UnimplementedStore provides no-op implementations of every Store method
// added in the runtime-controls release (PR #310). External implementers
// that maintain their own Store backend can embed this type to keep
// compiling when the interface grows, then override individual methods
// they actually support.
//
// This type exists for backwards compatibility with implementers outside
// this repository; the bundled SQLite/Postgres backends implement every
// method directly and should NOT embed it.
type UnimplementedStore struct{}

// Agent runtime settings & description.
func (UnimplementedStore) UpdateAgentDescription(ctx context.Context, agentID, userID, description string) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetAgentRuntimeSettings(ctx context.Context, agentID string) (*AgentRuntimeSettings, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) UpsertAgentRuntimeSettings(ctx context.Context, settings *AgentRuntimeSettings) error {
	return ErrNotImplemented
}

// Activity mutes (audit feed filtering).
func (UnimplementedStore) CreateActivityMute(ctx context.Context, mute *ActivityMute) error {
	return ErrNotImplemented
}
func (UnimplementedStore) ListActivityMutes(ctx context.Context, userID string) ([]*ActivityMute, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) DeleteActivityMute(ctx context.Context, id, userID string) error {
	return ErrNotImplemented
}

// Canonical approval records.
func (UnimplementedStore) CreateApprovalRecord(ctx context.Context, rec *ApprovalRecord) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetApprovalRecord(ctx context.Context, id string) (*ApprovalRecord, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) GetApprovalRecordByRequestID(ctx context.Context, requestID string) (*ApprovalRecord, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListPendingApprovalRecords(ctx context.Context, userID string) ([]*ApprovalRecord, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ClearApprovalRecordRequestID(ctx context.Context, id string) error {
	return ErrNotImplemented
}
func (UnimplementedStore) ResolveApprovalRecord(ctx context.Context, id, resolution, status string, resolvedAt time.Time) error {
	return ErrNotImplemented
}

// Runtime sessions and events.
func (UnimplementedStore) CreateRuntimeSession(ctx context.Context, sess *RuntimeSession) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetRuntimeSession(ctx context.Context, id string) (*RuntimeSession, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) GetRuntimeSessionByProxyBearerSecretHash(ctx context.Context, secretHash string) (*RuntimeSession, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListRuntimeSessionsByAgent(ctx context.Context, agentID string) ([]*RuntimeSession, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) RevokeRuntimeSession(ctx context.Context, id string, revokedAt time.Time) error {
	return ErrNotImplemented
}
func (UnimplementedStore) UpdateRuntimeSessionExpiry(ctx context.Context, id string, expiresAt time.Time) error {
	return ErrNotImplemented
}
func (UnimplementedStore) CreateRuntimeEvent(ctx context.Context, event *RuntimeEvent) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetRuntimeEvent(ctx context.Context, id string) (*RuntimeEvent, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListRuntimeEvents(ctx context.Context, userID string, filter RuntimeEventFilter) ([]*RuntimeEvent, error) {
	return nil, ErrNotImplemented
}

// Runtime policy rules.
func (UnimplementedStore) CreateRuntimePolicyRule(ctx context.Context, rule *RuntimePolicyRule) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetRuntimePolicyRule(ctx context.Context, id string) (*RuntimePolicyRule, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListRuntimePolicyRules(ctx context.Context, userID string, filter RuntimePolicyRuleFilter) ([]*RuntimePolicyRule, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) UpdateRuntimePolicyRule(ctx context.Context, rule *RuntimePolicyRule) error {
	return ErrNotImplemented
}
func (UnimplementedStore) DeleteRuntimePolicyRule(ctx context.Context, id, userID string) error {
	return ErrNotImplemented
}
func (UnimplementedStore) TouchRuntimePolicyRule(ctx context.Context, id string, matchedAt time.Time) error {
	return ErrNotImplemented
}

// Runtime credential placeholders and authorizations.
func (UnimplementedStore) CreateRuntimePlaceholder(ctx context.Context, placeholder *RuntimePlaceholder) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetRuntimePlaceholder(ctx context.Context, placeholder string) (*RuntimePlaceholder, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListRuntimePlaceholders(ctx context.Context, userID string) ([]*RuntimePlaceholder, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) DeleteRuntimePlaceholder(ctx context.Context, placeholder, userID string) error {
	return ErrNotImplemented
}
func (UnimplementedStore) TouchRuntimePlaceholder(ctx context.Context, placeholder string, usedAt time.Time) error {
	return ErrNotImplemented
}
func (UnimplementedStore) CreateCredentialAuthorization(ctx context.Context, auth *CredentialAuthorization) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetCredentialAuthorization(ctx context.Context, id string) (*CredentialAuthorization, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ConsumeMatchingCredentialAuthorization(ctx context.Context, match CredentialAuthorizationMatch, now time.Time) (*CredentialAuthorization, error) {
	return nil, ErrNotImplemented
}

// Runtime one-off approvals.
func (UnimplementedStore) CreateOneOffApproval(ctx context.Context, approval *OneOffApproval) error {
	return ErrNotImplemented
}
func (UnimplementedStore) ConsumeOneOffApproval(ctx context.Context, sessionID, requestFingerprint string, now time.Time) (*OneOffApproval, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ConsumeAgentOneOffApproval(ctx context.Context, agentID, requestFingerprint string, now time.Time) (*OneOffApproval, error) {
	return nil, ErrNotImplemented
}

// Tool execution leases.
func (UnimplementedStore) CreateToolExecutionLease(ctx context.Context, lease *ToolExecutionLease) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetToolExecutionLease(ctx context.Context, leaseID string) (*ToolExecutionLease, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) ListOpenToolExecutionLeases(ctx context.Context, sessionID string) ([]*ToolExecutionLease, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) CloseToolExecutionLease(ctx context.Context, leaseID string, closedAt time.Time, status string) error {
	return ErrNotImplemented
}

// Runtime task attribution.
func (UnimplementedStore) CreateTaskInvocation(ctx context.Context, inv *TaskInvocation) error {
	return ErrNotImplemented
}
func (UnimplementedStore) CreateTaskCall(ctx context.Context, call *TaskCall) error {
	return ErrNotImplemented
}
func (UnimplementedStore) UpsertActiveTaskSession(ctx context.Context, sess *ActiveTaskSession) error {
	return ErrNotImplemented
}
func (UnimplementedStore) GetActiveTaskSession(ctx context.Context, taskID, sessionID string) (*ActiveTaskSession, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) EndActiveTaskSession(ctx context.Context, taskID, sessionID string, endedAt time.Time, status string) error {
	return ErrNotImplemented
}

// Runtime preset decisions.
func (UnimplementedStore) GetRuntimePresetDecision(ctx context.Context, userID, commandKey, profile string) (*RuntimePresetDecision, error) {
	return nil, ErrNotImplemented
}
func (UnimplementedStore) UpsertRuntimePresetDecision(ctx context.Context, decision *RuntimePresetDecision) error {
	return ErrNotImplemented
}
