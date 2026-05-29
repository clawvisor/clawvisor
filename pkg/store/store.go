package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: record not found")

// ErrConflict is returned when a uniqueness constraint is violated.
var ErrConflict = errors.New("store: record already exists")

// ErrAmbiguous is returned by request-id-only lookups (GetPendingApproval,
// GetApprovalRecordByRequestID) when more than one row matches under the
// symmetric (user_id, request_id, COALESCE(task_id,”)) dedup scope.
// Callers must disambiguate via the *ByTask variant, or surface 409 to the
// client with the candidate task_ids enumerated via List*ByRequestID.
var ErrAmbiguous = errors.New("store: multiple records match without task scope")

// Store is the primary data access interface. All database operations go
// through this interface; no direct queries are made outside the store package.
type Store interface {
	// Users
	CreateUser(ctx context.Context, email, passwordHash string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	UpdateUserPassword(ctx context.Context, userID, newPasswordHash string) error
	DeleteUser(ctx context.Context, userID string) error
	CountUsers(ctx context.Context) (int, error)

	// Restrictions
	CreateRestriction(ctx context.Context, r *Restriction) (*Restriction, error)
	DeleteRestriction(ctx context.Context, id, userID string) error
	ListRestrictions(ctx context.Context, userID string) ([]*Restriction, error)
	MatchRestriction(ctx context.Context, userID, service, action string) (*Restriction, error)

	// Agents
	CreateAgent(ctx context.Context, userID, name, tokenHash string) (*Agent, error)
	CreateAgentWithOrg(ctx context.Context, userID, name, tokenHash, orgID string) (*Agent, error)
	// CreateAgentWithExpiry creates an agent whose token expires at the
	// given time. Used by the MCP OAuth and relay-pairing flows so leaked
	// tokens have finite blast radius. Pass a zero time to mean "no expiry"
	// (equivalent to CreateAgent).
	CreateAgentWithExpiry(ctx context.Context, userID, name, tokenHash string, expiresAt time.Time) (*Agent, error)
	GetAgentByToken(ctx context.Context, tokenHash string) (*Agent, error)
	GetAgent(ctx context.Context, agentID string) (*Agent, error)
	ListAgents(ctx context.Context, userID string) ([]*Agent, error)
	UpdateAgentDescription(ctx context.Context, agentID, userID, description string) error
	GetAgentRuntimeSettings(ctx context.Context, agentID string) (*AgentRuntimeSettings, error)
	UpsertAgentRuntimeSettings(ctx context.Context, settings *AgentRuntimeSettings) error
	DeleteAgent(ctx context.Context, id, userID string) error
	RotateAgentToken(ctx context.Context, id, userID, newTokenHash string) error
	SetAgentCallbackSecret(ctx context.Context, agentID, secret string) error
	GetAgentCallbackSecret(ctx context.Context, agentID string) (string, error)

	// Agent-group pairings (Telegram auto-approval)
	CreateAgentGroupPairing(ctx context.Context, userID, agentID, groupChatID string) error
	GetAgentGroupChatID(ctx context.Context, agentID string) (string, error)
	ListAgentIDsByGroup(ctx context.Context, groupChatID string) ([]string, error)
	DeleteAgentGroupPairing(ctx context.Context, agentID string) error
	DeleteAgentGroupPairingsByGroup(ctx context.Context, groupChatID string) error

	// Telegram groups (multi-group observation with per-group settings)
	CreateTelegramGroup(ctx context.Context, userID, groupChatID, title string) (*TelegramGroup, error)
	GetTelegramGroup(ctx context.Context, userID, groupChatID string) (*TelegramGroup, error)
	ListTelegramGroups(ctx context.Context, userID string) ([]*TelegramGroup, error)
	ListAllTelegramGroups(ctx context.Context) ([]*TelegramGroup, error)
	UpdateTelegramGroupAutoApproval(ctx context.Context, userID, groupChatID string, enabled bool, notify *bool) error
	DeleteTelegramGroup(ctx context.Context, userID, groupChatID string) error

	// Sessions (refresh tokens)
	CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*Session, error)
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	// ConsumeSession atomically deletes the session row matching tokenHash
	// and returns the row that was deleted. ErrNotFound means another caller
	// already consumed it (or the token never existed). Use this on the
	// refresh path so a stolen refresh token replayed concurrently can
	// produce at most one new token pair.
	ConsumeSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteUserSessions(ctx context.Context, userID string) error

	// Service credentials metadata (vault stores the actual bytes)
	UpsertServiceMeta(ctx context.Context, userID, serviceID, alias string, activatedAt time.Time) error
	GetServiceMeta(ctx context.Context, userID, serviceID, alias string) (*ServiceMeta, error)
	ListServiceMetas(ctx context.Context, userID string) ([]*ServiceMeta, error)
	DeleteServiceMeta(ctx context.Context, userID, serviceID, alias string) error
	CountServiceMetasByType(ctx context.Context, userID, serviceID string) (int, error)

	// Service configs (per-user variable values for configurable adapters)
	UpsertServiceConfig(ctx context.Context, userID, serviceID, alias string, config json.RawMessage) error
	GetServiceConfig(ctx context.Context, userID, serviceID, alias string) (*ServiceConfig, error)
	DeleteServiceConfig(ctx context.Context, userID, serviceID, alias string) error

	// MCP tool caches (per-user, populated at service activation; lazy-loaded
	// by the registry resolver on cache miss). Tools are an opaque JSON blob
	// — the store does not interpret them.
	UpsertMCPTools(ctx context.Context, userID, serviceID, alias string, tools json.RawMessage) error
	GetMCPTools(ctx context.Context, userID, serviceID, alias string) (json.RawMessage, error)
	DeleteMCPTools(ctx context.Context, userID, serviceID, alias string) error

	// Notification configs
	UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error
	GetNotificationConfig(ctx context.Context, userID, channel string) (*NotificationConfig, error)
	DeleteNotificationConfig(ctx context.Context, userID, channel string) error
	ListNotificationConfigsByChannel(ctx context.Context, channel string) ([]NotificationConfig, error)

	// Gateway request log (append-only backup)
	LogGatewayRequest(ctx context.Context, entry *GatewayRequestLog) error

	// Audit log
	//
	// LogAudit inserts an audit row. If entry.DedupedOf is nil the row is
	// treated as canonical. Request-level canonical rows leave DedupKey empty
	// and are unique on (user_id, request_id, COALESCE(task_id,'')); child
	// canonical rows set DedupKey and are unique on
	// (user_id, request_id, COALESCE(task_id,''), dedup_key). A collision
	// returns ErrConflict. The canonical-insertion sites in
	// handlers/gateway.go gate side effects on a prior FindDedupCandidate
	// check, so an ErrConflict here means two workers both passed that
	// check and raced — the loser should look the winner up via
	// FindDedupCandidate and surface its outcome instead of re-running.
	// Rows with DedupedOf set are dedup-attempt rows and are outside the
	// unique index.
	LogAudit(ctx context.Context, entry *AuditEntry) error
	UpdateAuditOutcome(ctx context.Context, id, outcome, errMsg string, durationMS int) error
	GetAuditEntry(ctx context.Context, id, userID string) (*AuditEntry, error)
	// GetAuditEntryByRequestID returns the latest request-level canonical audit
	// entry for (request_id, user_id). Used by the polling endpoint and other
	// callers that don't have task context. Child audit observations with
	// DedupKey set are excluded so per-tool history cannot shadow the request
	// outcome.
	GetAuditEntryByRequestID(ctx context.Context, requestID, userID string) (*AuditEntry, error)
	// GetAuditEntryByRequestIDAndTask returns the request-level canonical audit
	// entry for (request_id, user_id, task_id). Exact task_id matches win over
	// pre-task (task_id IS NULL) fallback; within that tier this getter returns
	// the newest row for status/feedback consumers.
	GetAuditEntryByRequestIDAndTask(ctx context.Context, requestID, userID, taskID string) (*AuditEntry, error)
	// FindDedupCandidate returns the request-level canonical audit entry that a new
	// (request_id, user_id, task_id) request should dedup against, or
	// ErrNotFound if no candidate exists. Exact task-scoped canonicals win over
	// pre-task fallback; within a tier the oldest row wins. Child audit
	// observations with DedupKey set are excluded.
	FindDedupCandidate(ctx context.Context, requestID, userID, taskID string) (*AuditEntry, error)
	ListAuditEntries(ctx context.Context, userID string, filter AuditFilter) ([]*AuditEntry, int, error)
	AuditActivityBuckets(ctx context.Context, userID string, since time.Time, bucketMinutes int) ([]ActivityBucket, error)

	// LLM request cost tracking. RecordLLMRequestCost stores one row per
	// upstream LLM call so per-task / per-user spend can be aggregated
	// without scanning audit_log. GetTaskCost rolls up by model for one
	// task.
	RecordLLMRequestCost(ctx context.Context, cost *LLMRequestCost) error
	GetTaskCost(ctx context.Context, userID, taskID string) (*TaskCostSummary, error)
	CreateActivityMute(ctx context.Context, mute *ActivityMute) error
	ListActivityMutes(ctx context.Context, userID string) ([]*ActivityMute, error)
	DeleteActivityMute(ctx context.Context, id, userID string) error

	// Tasks
	CreateTask(ctx context.Context, task *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, userID string, filter TaskFilter) ([]*Task, int, error)
	UpdateTaskStatus(ctx context.Context, id, status string) error
	UpdateTaskApproved(ctx context.Context, id string, expiresAt time.Time, authorizedActions []TaskAction) error
	// UpdateTaskStatusFrom atomically transitions a task from fromStatus to
	// toStatus. Returns true if the caller won the race, false if the row
	// was not in fromStatus (already approved/denied/expired by another
	// caller). Use this for any approve/deny/expand path that can be raced
	// across UI, Telegram, and API channels.
	UpdateTaskStatusFrom(ctx context.Context, id, fromStatus, toStatus string) (bool, error)
	// UpdateTaskApprovedFrom atomically promotes a task from fromStatus to
	// "active" with approved_at/expires_at/authorized_actions set, returning
	// true on win. Replaces UpdateTaskApproved for race-prone code paths.
	UpdateTaskApprovedFrom(ctx context.Context, id, fromStatus string, expiresAt time.Time, authorizedActions []TaskAction) (bool, error)
	UpdateTaskAuthorizedActions(ctx context.Context, id string, actions []TaskAction) error
	UpdateTaskActions(ctx context.Context, id string, actions []TaskAction, expiresAt time.Time) error
	IncrementTaskRequestCount(ctx context.Context, id string) error
	SetTaskPendingExpansion(ctx context.Context, id string, action *TaskAction, reason string) error
	ListExpiredTasks(ctx context.Context) ([]*Task, error)
	// ListExpiredInlineChatPendingTasks returns chat-bound pending
	// tasks (status='pending_approval', approval_source='inline_chat')
	// whose llmproxy cache hold has lapsed, i.e. created_at < cutoff.
	// Used by the approval sweeper to auto-deny tasks the user
	// abandoned in the chat surface (the cache hold expired so the
	// chat path can no longer resolve it; dashboard is gated by the
	// CHAT_APPROVAL_REQUIRED guard, so without this they sit forever).
	ListExpiredInlineChatPendingTasks(ctx context.Context, cutoff time.Time) ([]*Task, error)
	RevokeTask(ctx context.Context, id, userID string) error
	RevokeTasksByAgent(ctx context.Context, agentID, userID string) (int, error)

	// Pending approvals
	//
	// Under the symmetric dedup scope (user_id, request_id,
	// COALESCE(task_id,'')), two pending approvals with the same request_id
	// but different task_ids may coexist for one user. Lookups that take
	// only request_id return ErrAmbiguous in that case; callers must
	// disambiguate via the *ByTask variant or expose the ambiguity to the
	// client via ListPendingApprovalsByRequestID.
	SavePendingApproval(ctx context.Context, pa *PendingApproval) error
	// GetPendingApproval returns the unique pending approval for
	// (request_id, user_id). Returns ErrNotFound if no row matches and
	// ErrAmbiguous if more than one matches.
	GetPendingApproval(ctx context.Context, requestID, userID string) (*PendingApproval, error)
	// GetPendingApprovalByTask returns the pending approval scoped to an
	// exact (request_id, user_id, task_id). taskID == "" matches the
	// pre-task scope (task_id IS NULL in SQL).
	GetPendingApprovalByTask(ctx context.Context, requestID, userID, taskID string) (*PendingApproval, error)
	// ListPendingApprovalsByRequestID returns every pending approval that
	// matches (request_id, user_id). Used by the approval HTTP handlers to
	// surface candidate task_ids in a 409 AMBIGUOUS response when a caller
	// addresses a request_id-only endpoint and more than one row exists.
	ListPendingApprovalsByRequestID(ctx context.Context, requestID, userID string) ([]*PendingApproval, error)
	ListPendingApprovals(ctx context.Context, userID string) ([]*PendingApproval, error)
	// DeletePendingApproval removes the unique pending approval for
	// (request_id, user_id, task_id). Pass taskID == "" for the pre-task
	// scope. Callers must always supply task_id (typically read from a
	// prior Get) so the delete cannot accidentally fan out across siblings.
	DeletePendingApproval(ctx context.Context, requestID, userID, taskID string) error
	ListExpiredPendingApprovals(ctx context.Context) ([]*PendingApproval, error)
	UpdatePendingApprovalStatus(ctx context.Context, requestID, userID, taskID, status string) error
	// UpdatePendingApprovalStatusFrom atomically transitions a pending
	// approval from fromStatus to toStatus, scoped to a specific row by
	// (request_id, user_id, task_id). Returns true if the caller won the
	// race, false if the row was not in fromStatus. Use this for any
	// approve/deny path that can be raced across UI/Telegram/API.
	UpdatePendingApprovalStatusFrom(ctx context.Context, requestID, userID, taskID, fromStatus, toStatus string) (bool, error)
	// ClaimPendingApprovalForExecution atomically transitions a pending approval
	// from "approved" to "executing". Returns true if the caller won the claim,
	// false if another caller already claimed it (or the row is not "approved").
	ClaimPendingApprovalForExecution(ctx context.Context, requestID, userID, taskID string) (bool, error)
	// ListStalledExecutingApprovals returns rows stuck in "executing" beyond
	// leaseTTL — the recovery hook for daemon crashes mid-execution.
	ListStalledExecutingApprovals(ctx context.Context, leaseTTL time.Duration) ([]*PendingApproval, error)
	// ClaimStalledExecutingApprovalForRecovery atomically deletes a stalled
	// 'executing' row only if it is still in 'executing' status and still
	// past the lease cutoff. Returns true if the recovery sweeper won —
	// callers must dispatch the timeout callback only on true. Returns
	// false (no error) when the executor finished between list and claim
	// or another sweep already won the race.
	ClaimStalledExecutingApprovalForRecovery(ctx context.Context, requestID, userID, taskID string, leaseTTL time.Duration) (bool, error)

	// CreateApprovalRecordWithPending writes the canonical approval record
	// and its corresponding pending approval row in a single transaction.
	// Both rows commit together or neither commits — preventing the
	// orphan-canonical-record bug where the second insert failed and the
	// approval was visible in /api/approvals but had no executable pending
	// request to back it.
	CreateApprovalRecordWithPending(ctx context.Context, rec *ApprovalRecord, pa *PendingApproval) error

	// Canonical approval records
	CreateApprovalRecord(ctx context.Context, rec *ApprovalRecord) error
	GetApprovalRecord(ctx context.Context, id string) (*ApprovalRecord, error)
	// GetApprovalRecordByRequestID returns the unique approval record for
	// (request_id, user_id). Returns ErrNotFound if no row matches and
	// ErrAmbiguous if more than one matches under the symmetric dedup
	// scope.
	GetApprovalRecordByRequestID(ctx context.Context, requestID, userID string) (*ApprovalRecord, error)
	// GetApprovalRecordByRequestIDAndTask returns the approval record
	// scoped to an exact (request_id, user_id, task_id). taskID == "" means
	// pre-task (task_id IS NULL).
	GetApprovalRecordByRequestIDAndTask(ctx context.Context, requestID, userID, taskID string) (*ApprovalRecord, error)
	ListPendingApprovalRecords(ctx context.Context, userID string) ([]*ApprovalRecord, error)
	ClearApprovalRecordRequestID(ctx context.Context, id string) error
	ResolveApprovalRecord(ctx context.Context, id, resolution, status string, resolvedAt time.Time) error

	// Runtime sessions
	CreateRuntimeSession(ctx context.Context, sess *RuntimeSession) error
	GetRuntimeSession(ctx context.Context, id string) (*RuntimeSession, error)
	GetRuntimeSessionByProxyBearerSecretHash(ctx context.Context, secretHash string) (*RuntimeSession, error)
	ListRuntimeSessionsByAgent(ctx context.Context, agentID string) ([]*RuntimeSession, error)
	RevokeRuntimeSession(ctx context.Context, id string, revokedAt time.Time) error
	UpdateRuntimeSessionExpiry(ctx context.Context, id string, expiresAt time.Time) error
	CreateRuntimeEvent(ctx context.Context, event *RuntimeEvent) error
	GetRuntimeEvent(ctx context.Context, id string) (*RuntimeEvent, error)
	ListRuntimeEvents(ctx context.Context, userID string, filter RuntimeEventFilter) ([]*RuntimeEvent, error)
	CreateRuntimePolicyRule(ctx context.Context, rule *RuntimePolicyRule) error
	GetRuntimePolicyRule(ctx context.Context, id string) (*RuntimePolicyRule, error)
	ListRuntimePolicyRules(ctx context.Context, userID string, filter RuntimePolicyRuleFilter) ([]*RuntimePolicyRule, error)
	UpdateRuntimePolicyRule(ctx context.Context, rule *RuntimePolicyRule) error
	DeleteRuntimePolicyRule(ctx context.Context, id, userID string) error
	TouchRuntimePolicyRule(ctx context.Context, id string, matchedAt time.Time) error

	// Runtime credential placeholders
	CreateRuntimePlaceholder(ctx context.Context, placeholder *RuntimePlaceholder) error
	GetRuntimePlaceholder(ctx context.Context, placeholder string) (*RuntimePlaceholder, error)
	ListRuntimePlaceholders(ctx context.Context, userID string) ([]*RuntimePlaceholder, error)
	DeleteRuntimePlaceholder(ctx context.Context, placeholder, userID string) error
	TouchRuntimePlaceholder(ctx context.Context, placeholder string, usedAt time.Time) error
	CreateCredentialAuthorization(ctx context.Context, auth *CredentialAuthorization) error
	GetCredentialAuthorization(ctx context.Context, id string) (*CredentialAuthorization, error)
	ConsumeMatchingCredentialAuthorization(ctx context.Context, match CredentialAuthorizationMatch, now time.Time) (*CredentialAuthorization, error)
	DeleteCredentialAuthorization(ctx context.Context, id, userID string) error

	// Runtime one-off approvals
	CreateOneOffApproval(ctx context.Context, approval *OneOffApproval) error
	ConsumeOneOffApproval(ctx context.Context, sessionID, requestFingerprint string, now time.Time) (*OneOffApproval, error)
	ConsumeAgentOneOffApproval(ctx context.Context, agentID, requestFingerprint string, now time.Time) (*OneOffApproval, error)

	// Runtime leases
	CreateToolExecutionLease(ctx context.Context, lease *ToolExecutionLease) error
	GetToolExecutionLease(ctx context.Context, leaseID string) (*ToolExecutionLease, error)
	ListOpenToolExecutionLeases(ctx context.Context, sessionID string) ([]*ToolExecutionLease, error)
	CloseToolExecutionLease(ctx context.Context, leaseID string, closedAt time.Time, status string) error

	// Runtime task attribution
	CreateTaskInvocation(ctx context.Context, inv *TaskInvocation) error
	CreateTaskCall(ctx context.Context, call *TaskCall) error
	UpsertActiveTaskSession(ctx context.Context, sess *ActiveTaskSession) error
	GetActiveTaskSession(ctx context.Context, taskID, sessionID string) (*ActiveTaskSession, error)
	EndActiveTaskSession(ctx context.Context, taskID, sessionID string, endedAt time.Time, status string) error

	// Runtime preset decisions
	GetRuntimePresetDecision(ctx context.Context, userID, commandKey, profile string) (*RuntimePresetDecision, error)
	UpsertRuntimePresetDecision(ctx context.Context, decision *RuntimePresetDecision) error

	// Notification messages (cross-channel message tracking)
	SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error
	GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error)

	// Chain facts (intent verification context chaining)
	SaveChainFacts(ctx context.Context, facts []*ChainFact) error
	ListChainFacts(ctx context.Context, taskID, sessionID string, limit int) ([]*ChainFact, error)
	ChainFactValueExists(ctx context.Context, taskID, sessionID, value string) (bool, error)
	DeleteChainFactsByTask(ctx context.Context, taskID string) error

	// Paired devices (mobile push notifications)
	CreatePairedDevice(ctx context.Context, d *PairedDevice) error
	GetPairedDevice(ctx context.Context, id string) (*PairedDevice, error)
	ListPairedDevices(ctx context.Context, userID string) ([]*PairedDevice, error)
	DeletePairedDevice(ctx context.Context, id string) error
	ListPairedDevicesByDeviceToken(ctx context.Context, deviceToken string) ([]*PairedDevice, error)
	UpdatePairedDeviceLastSeen(ctx context.Context, id string) error
	UpdatePairedDevicePushToStartToken(ctx context.Context, id, token string) error

	// Connection requests (daemon agent onboarding)
	CreateConnectionRequest(ctx context.Context, req *ConnectionRequest) error
	GetConnectionRequest(ctx context.Context, id string) (*ConnectionRequest, error)
	ListPendingConnectionRequests(ctx context.Context, userID string) ([]*ConnectionRequest, error)
	UpdateConnectionRequestStatus(ctx context.Context, id, status, agentID string) error
	// UpdateConnectionRequestStatusIfPending transitions the row only if
	// its current status is "pending". Returns true when the row was
	// modified, false when another writer beat us (status was already
	// approved/denied/expired). Lets timeout-style callers expire a
	// pending request without clobbering an approval that landed in the
	// race window.
	UpdateConnectionRequestStatusIfPending(ctx context.Context, id, status string) (modified bool, err error)
	DeleteExpiredConnectionRequests(ctx context.Context) error
	CountPendingConnectionRequestsForUser(ctx context.Context, userID string) (int, error)

	// Generated adapters (cloud-safe persistence for LLM-generated YAML definitions)
	SaveGeneratedAdapter(ctx context.Context, userID, serviceID, yamlContent string) error
	ListGeneratedAdapters(ctx context.Context, userID string) ([]*GeneratedAdapter, error)
	DeleteGeneratedAdapter(ctx context.Context, userID, serviceID string) error

	// MCP sessions (persist across restarts)
	CreateMCPSession(ctx context.Context, id string, expiresAt time.Time) error
	MCPSessionValid(ctx context.Context, id string) (bool, error)
	CleanupMCPSessions(ctx context.Context) error

	// OAuth (MCP client registration + authorization codes)
	CreateOAuthClient(ctx context.Context, client *OAuthClient) error
	GetOAuthClient(ctx context.Context, clientID string) (*OAuthClient, error)
	SaveAuthorizationCode(ctx context.Context, code *OAuthAuthorizationCode) error
	// ConsumeAuthorizationCode atomically retrieves and deletes an authorization
	// code. Returns ErrNotFound if the code does not exist (or was already consumed).
	ConsumeAuthorizationCode(ctx context.Context, codeHash string) (*OAuthAuthorizationCode, error)

	// Agent feedback (bug reports and NPS)
	CreateFeedbackReport(ctx context.Context, report *FeedbackReport) error
	GetFeedbackReport(ctx context.Context, id string) (*FeedbackReport, error)
	ListFeedbackReports(ctx context.Context, userID string, limit, offset int) ([]*FeedbackReport, int, error)
	SaveNPSResponse(ctx context.Context, nps *NPSResponse) error
	GetAgentNPSStats(ctx context.Context, agentID string) (*NPSStats, error)
	GetAgentLastNPSTime(ctx context.Context, agentID string) (*time.Time, error)

	// Aggregate counts (telemetry)
	TelemetryCounts(ctx context.Context) (*TelemetryCounts, error)

	// Health
	Ping(ctx context.Context) error
	Close() error
}

// TelemetryCounts holds aggregate, anonymous usage data for telemetry.
type TelemetryCounts struct {
	Agents            int            // total registered agents
	RequestsByService map[string]int // gateway requests per service (e.g. "gmail": 120)
}

// User represents a registered Clawvisor account.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Risk-level constants for ConversationAutoApproveThreshold and the
// taskrisk classifier's risk_level field. Defined here (not in
// taskrisk) so pkg/store can validate threshold values without pulling
// in the taskrisk LLM client dependency.
const (
	ConversationAutoApproveOff      = "off"
	ConversationAutoApproveLow      = "low"
	ConversationAutoApproveMedium   = "medium"
	ConversationAutoApproveHigh     = "high"
	ConversationAutoApproveCritical = "critical"
)

// ConversationAutoApproveUICap is the highest threshold a user is
// allowed to set today via UI or API. The auto-approve gate's
// comparison code accepts any level (so a future product decision can
// relax this without touching the runtime), but write paths enforce
// the cap.
const ConversationAutoApproveUICap = ConversationAutoApproveMedium

// ValidateConversationAutoApproveThreshold normalizes and validates a
// candidate threshold value. Empty string collapses to "off" (the
// default). Any unknown value is rejected. When enforceUICap is true,
// values above ConversationAutoApproveUICap are rejected — this is the
// shape used by API handlers and the Store update method. When false,
// any documented level is accepted — used by tests and any future
// internal path that bypasses the product cap.
func ValidateConversationAutoApproveThreshold(raw string, enforceUICap bool) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		v = ConversationAutoApproveOff
	}
	rank, ok := conversationAutoApproveRank[v]
	if !ok {
		return "", fmt.Errorf("invalid conversation_auto_approve_threshold: %q", raw)
	}
	if enforceUICap && rank > conversationAutoApproveRank[ConversationAutoApproveUICap] {
		return "", fmt.Errorf("conversation_auto_approve_threshold %q exceeds UI cap %q", v, ConversationAutoApproveUICap)
	}
	return v, nil
}

// NormalizeConversationAutoApproveThreshold canonicalizes a threshold
// string to the form the migration stores: lowercased, trimmed, and
// with the empty string mapped to "off". Unknown values pass through
// unchanged so the upsert path doesn't silently rewrite an invalid
// value into a valid one — validation belongs in the API handler.
//
// Defense-in-depth: any value above ConversationAutoApproveUICap is
// clamped down to the cap. The API handler also enforces the cap via
// ValidateConversationAutoApproveThreshold(enforceUICap=true), but a
// store caller that bypasses the handler (test fixture, future SQL
// migration, internal admin tool) could otherwise persist an
// above-cap value and the runtime gate (ConversationAutoApproveCovers)
// would honor it. Clamping here makes the boundary enforce at the
// store layer too.
//
// Used by the sqlite + postgres upsert paths so the persisted value
// matches the migration default and any future `== "off"` string
// comparison doesn't have to defensively treat "" as equivalent.
func NormalizeConversationAutoApproveThreshold(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return ConversationAutoApproveOff
	}
	rank, ok := conversationAutoApproveRank[v]
	if !ok {
		return raw
	}
	if rank > conversationAutoApproveRank[ConversationAutoApproveUICap] {
		return ConversationAutoApproveUICap
	}
	return v
}

// conversationAutoApproveRank orders the threshold values so the gate
// can ask "is the assessed risk level <= the user's threshold?". "off"
// ranks below every level, so when threshold=="off" nothing is ever
// at-or-below it. Risk levels follow the taskrisk severity order.
var conversationAutoApproveRank = map[string]int{
	ConversationAutoApproveOff:      -1,
	ConversationAutoApproveLow:      0,
	ConversationAutoApproveMedium:   1,
	ConversationAutoApproveHigh:     2,
	ConversationAutoApproveCritical: 3,
}

// ConversationAutoApproveCovers reports whether an assessed risk_level
// is at-or-below the user's configured threshold. Unknown risk levels
// (e.g. "unknown" from the assessor) and an "off" threshold both
// produce false — the caller falls back to human approval. Comparison
// is case-insensitive on both sides.
func ConversationAutoApproveCovers(threshold, riskLevel string) bool {
	t := strings.ToLower(strings.TrimSpace(threshold))
	r := strings.ToLower(strings.TrimSpace(riskLevel))
	if t == "" || t == ConversationAutoApproveOff {
		return false
	}
	tRank, tOK := conversationAutoApproveRank[t]
	rRank, rOK := conversationAutoApproveRank[r]
	if !tOK || !rOK {
		return false
	}
	return rRank <= tRank
}

// Session holds a hashed refresh token.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// Agent is an AI agent that authenticates via a long-lived bearer token.
type Agent struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	TokenHash   string    `json:"-"`
	OrgID       string    `json:"org_id,omitempty"` // set by cloud when agent belongs to an org
	CreatedAt   time.Time `json:"created_at"`
	// TokenExpiresAt bounds the lifetime of a leaked bearer token. nil
	// means no expiry — preserved for legacy POST /api/agents tokens that
	// the user owns end-to-end. MCP OAuth and relay-pairing flows write a
	// non-nil value so a leaked token has finite blast radius. RequireAgent
	// rejects tokens whose expiry has passed.
	TokenExpiresAt  *time.Time            `json:"token_expires_at,omitempty"`
	ActiveTaskCount int                   `json:"active_task_count"`
	LastTaskAt      *time.Time            `json:"last_task_at,omitempty"`
	RuntimeSettings *AgentRuntimeSettings `json:"runtime_settings,omitempty"`
}

type AgentRuntimeSettings struct {
	AgentID                          string `json:"agent_id"`
	RuntimeEnabled                   bool   `json:"runtime_enabled"`
	RuntimeMode                      string `json:"runtime_mode"`
	StarterProfile                   string `json:"starter_profile"`
	OutboundCredentialMode           string `json:"outbound_credential_mode"`
	InjectStoredBearer               bool   `json:"inject_stored_bearer"`
	LiteProxySecretDetectionDisabled bool   `json:"lite_proxy_secret_detection_disabled"`
	// ConversationAutoApproveThreshold caps the risk level at which
	// conversation-based auto-approval of inline task creation will
	// skip the human approval prompt for this agent. Values: "off"
	// (default — always prompt), "low", "medium" (UI cap), "high",
	// "critical" (theoretically supported, blocked at the API/UI
	// layer). The gate also requires the assessor to emit
	// intent_match="yes" and an empty conflicts array AND the runtime
	// to have extracted at least one genuine human turn from the
	// inbound transcript — threshold alone is not sufficient.
	ConversationAutoApproveThreshold string    `json:"conversation_auto_approve_threshold"`
	CreatedAt                        time.Time `json:"created_at"`
	UpdatedAt                        time.Time `json:"updated_at"`
}

// ServiceMeta records that a user has activated a given service.
// The actual credential bytes live in the vault.
type ServiceMeta struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	ServiceID   string    `json:"service_id"`
	Alias       string    `json:"alias"`
	ActivatedAt time.Time `json:"activated_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Restriction is a hard block on a service/action that no task can override.
type Restriction struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ServiceConfig stores per-user, per-service variable values for configurable adapters.
type ServiceConfig struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	ServiceID string          `json:"service_id"`
	Alias     string          `json:"alias"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// NotificationConfig stores per-user, per-channel notification settings.
type NotificationConfig struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	Channel   string          `json:"channel"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// GatewayRequestLog is an append-only backup record of every gateway request.
// Written before the primary audit insert so we retain visibility even if that
// insert is silently dropped.
type GatewayRequestLog struct {
	AuditID    string
	RequestID  string
	AgentID    string
	UserID     string
	Service    string
	Action     string
	TaskID     string
	Reason     string
	Decision   string
	Outcome    string
	DurationMS int
}

// AuditEntry is one row in the audit_log table.
type AuditEntry struct {
	ID                      string          `json:"id"`
	UserID                  string          `json:"user_id"`
	AgentID                 *string         `json:"agent_id,omitempty"`
	RequestID               string          `json:"request_id"`
	DedupKey                *string         `json:"-"`
	TaskID                  *string         `json:"task_id,omitempty"`
	SessionID               *string         `json:"session_id,omitempty"`
	ApprovalID              *string         `json:"approval_id,omitempty"`
	LeaseID                 *string         `json:"lease_id,omitempty"`
	ToolUseID               *string         `json:"tool_use_id,omitempty"`
	MatchedTaskID           *string         `json:"matched_task_id,omitempty"`
	LeaseTaskID             *string         `json:"lease_task_id,omitempty"`
	Timestamp               time.Time       `json:"timestamp"`
	Service                 string          `json:"service"`
	Action                  string          `json:"action"`
	ParamsSafe              json.RawMessage `json:"params_safe"`
	Decision                string          `json:"decision"`
	Outcome                 string          `json:"outcome"`
	PolicyID                *string         `json:"policy_id,omitempty"`
	RuleID                  *string         `json:"rule_id,omitempty"`
	ResolutionConfidence    *string         `json:"resolution_confidence,omitempty"`
	IntentVerdict           *string         `json:"intent_verdict,omitempty"`
	UsedActiveTaskContext   bool            `json:"used_active_task_context"`
	UsedLeaseBias           bool            `json:"used_lease_bias"`
	UsedConvJudgeResolution bool            `json:"used_conv_judge_resolution"`
	WouldBlock              bool            `json:"would_block"`
	WouldReview             bool            `json:"would_review"`
	WouldPromptInline       bool            `json:"would_prompt_inline"`
	SafetyFlagged           bool            `json:"safety_flagged"`
	SafetyReason            *string         `json:"safety_reason,omitempty"`
	Reason                  *string         `json:"reason,omitempty"`
	DataOrigin              *string         `json:"data_origin,omitempty"`
	ContextSrc              *string         `json:"context_src,omitempty"`
	DurationMS              int             `json:"duration_ms"`
	FiltersApplied          json.RawMessage `json:"filters_applied,omitempty"`
	Verification            json.RawMessage `json:"verification,omitempty"`
	ErrorMsg                *string         `json:"error_msg,omitempty"`
	// DedupedOf is set on retry-attempt rows to the id of the canonical
	// audit entry they shadow. Canonical rows have DedupedOf == nil.
	DedupedOf *string `json:"deduped_of,omitempty"`
}

// LLMRequestCost is one row per upstream LLM call recording the model,
// token breakdown, and computed cost. Lives in its own table (not on
// audit_log) so the majority of audit rows — tool_use, approvals,
// resolver swaps — don't carry mostly-NULL usage columns. AuditID is
// the FK back to the audit_log row that captured the request.
//
// CostMicros is int64 micro-USD (1e-6 USD per unit). Nil when the
// model isn't in the pricing table — token counts are still recorded
// so aggregates can surface "unknown-model spend" and cost is
// re-derivable when the table updates.
//
// Caller contract: UserID must equal the owning agent's UserID (the
// per-user cost rollup query in GetTaskCost filters by this column,
// so a mismatch would silently miscount). Today the only producer is
// the lite-proxy audit emitter, which derives UserID from
// agent.UserID; any future caller should preserve that invariant.
type LLMRequestCost struct {
	AuditID          string    `json:"audit_id"`
	UserID           string    `json:"user_id"`
	AgentID          *string   `json:"agent_id,omitempty"`
	TaskID           *string   `json:"task_id,omitempty"`
	RequestID        string    `json:"request_id"`
	Timestamp        time.Time `json:"timestamp"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	CostMicros       *int64    `json:"cost_micros,omitempty"`
}

// TaskCostSummary is the rollup of all LLMRequestCost rows for a
// single task. ByModel breaks the totals down per model so the UI
// can show "X spent on Opus, Y on Sonnet" without re-querying.
type TaskCostSummary struct {
	TaskID           string                  `json:"task_id"`
	RequestCount     int                     `json:"request_count"`
	InputTokens      int64                   `json:"input_tokens"`
	OutputTokens     int64                   `json:"output_tokens"`
	CacheReadTokens  int64                   `json:"cache_read_tokens"`
	CacheWriteTokens int64                   `json:"cache_write_tokens"`
	CostMicros       int64                   `json:"cost_micros"`
	// UnknownModels and ByModel both serialize as `[]` when empty so
	// consumers (TS client) get a consistent shape across all
	// summaries and don't have to branch on undefined-vs-empty-array.
	UnknownModels []string               `json:"unknown_models"`
	ByModel       []TaskCostByModelEntry `json:"by_model"`
}

// TaskCostByModelEntry is one model's contribution to a task's cost.
type TaskCostByModelEntry struct {
	Model            string `json:"model"`
	RequestCount     int    `json:"request_count"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CostMicros       int64  `json:"cost_micros"`
	Known            bool   `json:"known"`
}

// ActivityMute suppresses noisy runtime egress rows from the activity feed.
// Matching is host-exact with an optional path-prefix refinement.
type ActivityMute struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Host       string    `json:"host"`
	PathPrefix string    `json:"path_prefix,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// TaskAction represents a single authorized action within a task scope.
type TaskAction struct {
	Service            string          `json:"service"`
	Action             string          `json:"action"` // specific action or "*"
	AutoExecute        bool            `json:"auto_execute"`
	ResponseFilters    json.RawMessage `json:"response_filters,omitempty"`
	ExpectedUse        string          `json:"expected_use,omitempty"`
	ExpansionRationale string          `json:"expansion_rationale,omitempty"` // set from PendingReason when scope expansion is approved
	// Verification controls intent verification for this scope: "strict" (default), "lenient", "off".
	Verification string `json:"verification,omitempty"`
}

// PlannedCall is a concrete or templated API call that an agent declares at task
// creation time. Planned calls are evaluated during task risk assessment and shown
// to the user at approval time. At request time, calls that match a planned call
// skip intent verification.
//
// Matching rules:
//   - Service and Action must match exactly.
//   - Params must be non-empty (calls without params cannot skip verification
//     because we don't know what entity they target).
//   - Each param value is matched against the actual request:
//   - Literal value: must match exactly (JSON-normalized).
//   - "$chain": the actual value must appear in the task's chain context facts.
//     This lets agents template calls like {"thread_id": "$chain"} to mean
//     "any thread_id that was returned by a prior call in this task".
type PlannedCall struct {
	Service string         `json:"service"`
	Action  string         `json:"action"`
	Params  map[string]any `json:"params,omitempty"` // required for matching; "$chain" values match chain context
	Reason  string         `json:"reason"`           // why this call will be made
}

// Task represents a task-scoped authorization.
type Task struct {
	ID                     string          `json:"id"`
	UserID                 string          `json:"user_id"`
	AgentID                string          `json:"agent_id"`
	Purpose                string          `json:"purpose"`
	Status                 string          `json:"status"`   // pending_approval | active | completed | expired | denied | cancelled | pending_scope_expansion | revoked
	Lifetime               string          `json:"lifetime"` // session | standing
	AuthorizedActions      []TaskAction    `json:"authorized_actions"`
	PlannedCalls           []PlannedCall   `json:"planned_calls,omitempty"`
	ExpectedTools          json.RawMessage `json:"expected_tools,omitempty"`
	ExpectedEgress         json.RawMessage `json:"expected_egress,omitempty"`
	RequiredCredentials    json.RawMessage `json:"required_credentials,omitempty"`
	IntentVerificationMode string          `json:"intent_verification_mode,omitempty"`
	// ChainExtractionMode overrides the system default for async chain-context
	// extraction. "" (unset) defers to the system default; "full" runs the
	// LLM Phase-2 pass; "builtins_only" skips it (synchronous builtin regex
	// only). Resolved in internal/api/handlers/gateway.go.
	ChainExtractionMode string     `json:"chain_extraction_mode,omitempty"`
	ExpectedUse         string     `json:"expected_use,omitempty"`
	SchemaVersion       int        `json:"schema_version,omitempty"`
	CallbackURL         *string    `json:"callback_url,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	ExpiresInSeconds    int        `json:"expires_in_seconds,omitempty"`
	RequestCount        int        `json:"request_count"`
	// PendingAction holds the action awaiting scope expansion approval.
	PendingAction *TaskAction `json:"pending_action,omitempty"`
	PendingReason string      `json:"pending_reason,omitempty"`
	// RiskLevel is the LLM-assessed risk level ("low", "medium", "high", "critical", "unknown", or "").
	RiskLevel   string          `json:"risk_level,omitempty"`
	RiskDetails json.RawMessage `json:"risk_details,omitempty"`
	// ApprovalSource indicates how the task was approved ("",
	// "manual", "telegram_group", "telegram_button", "inline_chat").
	// "inline_chat" is also load-bearing pre-approval: it's set at
	// pending-creation time by CreatePendingInlineTask and gates
	// dashboard Approve/Deny (isInlineChatPending → 409
	// INLINE_CHAT_BOUND) plus the chat-bound expiry sweep.
	ApprovalSource    string          `json:"approval_source,omitempty"`
	ApprovalRationale json.RawMessage `json:"approval_rationale,omitempty"`
}

// PendingApproval is a gateway request awaiting human approval.
type PendingApproval struct {
	ID               string          `json:"id"`
	UserID           string          `json:"user_id"`
	RequestID        string          `json:"request_id"`
	TaskID           *string         `json:"task_id,omitempty"`
	AuditID          string          `json:"audit_id"`
	ApprovalRecordID *string         `json:"approval_record_id,omitempty"`
	RequestBlob      json.RawMessage `json:"request_blob"`
	CallbackURL      *string         `json:"callback_url,omitempty"`
	Status           string          `json:"status"` // "pending" or "approved"
	ExpiresAt        time.Time       `json:"expires_at"`
	CreatedAt        time.Time       `json:"created_at"`
}

// ApprovalRecord is the canonical approval object shared across surfaces.
type ApprovalRecord struct {
	ID                  string          `json:"id"`
	Kind                string          `json:"kind"`
	UserID              string          `json:"user_id"`
	AgentID             *string         `json:"agent_id,omitempty"`
	RequestID           *string         `json:"request_id,omitempty"`
	TaskID              *string         `json:"task_id,omitempty"`
	SessionID           *string         `json:"session_id,omitempty"`
	Status              string          `json:"status"`
	Surface             string          `json:"surface"`
	SummaryJSON         json.RawMessage `json:"summary_json,omitempty"`
	PayloadJSON         json.RawMessage `json:"payload_json,omitempty"`
	ResolutionTransport string          `json:"resolution_transport,omitempty"`
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	ResolvedAt          *time.Time      `json:"resolved_at,omitempty"`
	Resolution          string          `json:"resolution,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// RuntimeSession binds proxy-authenticated traffic to an agent run.
type RuntimeSession struct {
	ID                    string          `json:"id"`
	UserID                string          `json:"user_id"`
	AgentID               string          `json:"agent_id"`
	OrgID                 string          `json:"org_id,omitempty"`
	Mode                  string          `json:"mode"`
	ProxyBearerSecretHash string          `json:"proxy_bearer_secret_hash"`
	ObservationMode       bool            `json:"observation_mode"`
	MetadataJSON          json.RawMessage `json:"metadata_json,omitempty"`
	ExpiresAt             time.Time       `json:"expires_at"`
	CreatedAt             time.Time       `json:"created_at"`
	RevokedAt             *time.Time      `json:"revoked_at,omitempty"`
}

// RuntimeEvent is an append-only observability record for runtime decisions.
type RuntimeEvent struct {
	ID                  string          `json:"id"`
	Timestamp           time.Time       `json:"timestamp"`
	SessionID           string          `json:"session_id"`
	UserID              string          `json:"user_id"`
	AgentID             string          `json:"agent_id"`
	Provider            string          `json:"provider,omitempty"`
	EventType           string          `json:"event_type"`
	ActionKind          string          `json:"action_kind,omitempty"`
	ApprovalID          *string         `json:"approval_id,omitempty"`
	TaskID              *string         `json:"task_id,omitempty"`
	MatchedTaskID       *string         `json:"matched_task_id,omitempty"`
	LeaseID             *string         `json:"lease_id,omitempty"`
	ToolUseID           *string         `json:"tool_use_id,omitempty"`
	RequestFingerprint  *string         `json:"request_fingerprint,omitempty"`
	ResolutionTransport *string         `json:"resolution_transport,omitempty"`
	Decision            *string         `json:"decision,omitempty"`
	Outcome             *string         `json:"outcome,omitempty"`
	Reason              *string         `json:"reason,omitempty"`
	MetadataJSON        json.RawMessage `json:"metadata_json,omitempty"`
}

type RuntimePolicyRule struct {
	ID            string  `json:"id"`
	UserID        string  `json:"user_id"`
	AgentID       *string `json:"agent_id,omitempty"`
	Kind          string  `json:"kind"`
	Action        string  `json:"action"`
	Service       string  `json:"service,omitempty"`
	ServiceAction string  `json:"service_action,omitempty"`
	// Host and Path are kind-specific match/storage fields. For egress
	// rules they are request matchers; secret_suppression uses Host for
	// the secret fingerprint; secret_rewrite uses Host for the fingerprint
	// and Path for the runtime placeholder; passthrough uses Path for the
	// RFC3339 expiry. Keep new machine-owned rule kinds documented here
	// until they have dedicated metadata storage.
	Host          string          `json:"host,omitempty"`
	Method        string          `json:"method,omitempty"`
	Path          string          `json:"path,omitempty"`
	PathRegex     string          `json:"path_regex,omitempty"`
	HeadersShape  json.RawMessage `json:"headers_shape_json,omitempty"`
	BodyShape     json.RawMessage `json:"body_shape_json,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	InputShape    json.RawMessage `json:"input_shape_json,omitempty"`
	InputRegex    string          `json:"input_regex,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Source        string          `json:"source"`
	Enabled       bool            `json:"enabled"`
	LastMatchedAt *time.Time      `json:"last_matched_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type RuntimePolicyRuleFilter struct {
	AgentID string
	Kind    string
	Enabled *bool
	Limit   int
}

type RuntimePresetDecision struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	CommandKey string    `json:"command_key"`
	Profile    string    `json:"profile"`
	Decision   string    `json:"decision"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RuntimePlaceholder is an agent-scoped placeholder that resolves to an
// existing vault credential at proxy runtime.
type RuntimePlaceholder struct {
	Placeholder       string     `json:"placeholder"`
	UserID            string     `json:"user_id"`
	AgentID           string     `json:"agent_id,omitempty"`
	ServiceID         string     `json:"service_id"`
	VaultItemID       string     `json:"vault_item_id,omitempty"`
	CredentialGrantID string     `json:"credential_grant_id,omitempty"`
	TaskID            string     `json:"task_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	UseCount          int        `json:"use_count,omitempty"`
}

// CredentialAuthorization grants reuse of a previously reviewed outbound
// credential-bearing header without storing the raw credential itself.
type CredentialAuthorization struct {
	ID            string          `json:"id"`
	ApprovalID    *string         `json:"approval_id,omitempty"`
	UserID        string          `json:"user_id"`
	AgentID       string          `json:"agent_id"`
	SessionID     *string         `json:"session_id,omitempty"`
	Scope         string          `json:"scope"`
	CredentialRef string          `json:"credential_ref"`
	Service       string          `json:"service"`
	Host          string          `json:"host"`
	HeaderName    string          `json:"header_name"`
	Scheme        string          `json:"scheme"`
	Status        string          `json:"status"`
	MetadataJSON  json.RawMessage `json:"metadata_json,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	UsedAt        *time.Time      `json:"used_at,omitempty"`
	LastMatchedAt *time.Time      `json:"last_matched_at,omitempty"`
}

type CredentialAuthorizationMatch struct {
	UserID        string
	AgentID       string
	SessionID     string
	CredentialRef string
	Service       string
	Host          string
	HeaderName    string
	Scheme        string
}

// OneOffApproval is a single-use retry artifact for blocked runtime requests.
type OneOffApproval struct {
	ID                 string     `json:"id"`
	SessionID          string     `json:"session_id"`
	RequestFingerprint string     `json:"request_fingerprint"`
	ApprovalID         *string    `json:"approval_id,omitempty"`
	ApprovedAt         time.Time  `json:"approved_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	UsedAt             *time.Time `json:"used_at,omitempty"`
}

// ToolExecutionLease is the runtime context opened when a tool call is released.
type ToolExecutionLease struct {
	LeaseID      string          `json:"lease_id"`
	SessionID    string          `json:"session_id"`
	TaskID       string          `json:"task_id"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolName     string          `json:"tool_name"`
	Status       string          `json:"status"`
	MetadataJSON json.RawMessage `json:"metadata_json,omitempty"`
	OpenedAt     time.Time       `json:"opened_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	ClosedAt     *time.Time      `json:"closed_at,omitempty"`
}

// TaskInvocation records a task-scoped execution attempt or session start.
type TaskInvocation struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	SessionID      string          `json:"session_id"`
	UserID         string          `json:"user_id"`
	AgentID        string          `json:"agent_id"`
	RequestID      string          `json:"request_id,omitempty"`
	InvocationType string          `json:"invocation_type"`
	Status         string          `json:"status"`
	MetadataJSON   json.RawMessage `json:"metadata_json,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

// TaskCall records one task-attributed tool or egress action.
type TaskCall struct {
	ID           string          `json:"id"`
	TaskID       string          `json:"task_id"`
	InvocationID string          `json:"invocation_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	Service      string          `json:"service"`
	Action       string          `json:"action"`
	Outcome      string          `json:"outcome,omitempty"`
	ApprovalID   *string         `json:"approval_id,omitempty"`
	AuditID      *string         `json:"audit_id,omitempty"`
	MetadataJSON json.RawMessage `json:"metadata_json,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ActiveTaskSession tracks live task context for a runtime session.
type ActiveTaskSession struct {
	ID           string          `json:"id"`
	TaskID       string          `json:"task_id"`
	SessionID    string          `json:"session_id"`
	UserID       string          `json:"user_id"`
	AgentID      string          `json:"agent_id"`
	Status       string          `json:"status"`
	MetadataJSON json.RawMessage `json:"metadata_json,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	LastSeenAt   time.Time       `json:"last_seen_at"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
}

type RuntimeEventFilter struct {
	SessionID string
	EventType string
	Limit     int
}

// TaskFilter controls which tasks are returned by ListTasks.
// Zero values mean "no filter" (backwards compatible).
type TaskFilter struct {
	ActiveOnly bool   // status IN ('active','pending_approval','pending_scope_expansion')
	Status     string // exact status match (e.g. "active", "pending_approval", "denied"); empty = no filter
	Limit      int    // 0 -> no limit
	Offset     int
}

// AuditFilter controls which entries are returned by ListAuditEntries.
// Zero values mean "no filter" for that field.
type AuditFilter struct {
	Service        string // filter by service
	Outcome        string // filter by outcome
	DataOrigin     string // filter by data_origin
	TaskID         string // filter by task_id
	AgentID        string // filter by agent_id
	IncludeRuntime *bool  // nil -> default include, false -> suppress runtime.* rows
	Limit          int    // 0 -> default (50)
	Offset         int
}

// ActivityBucket is one row of the aggregated audit activity histogram.
type ActivityBucket struct {
	Bucket  time.Time `json:"bucket"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}

// ChainFact is a structural reference extracted from an adapter result for
// chain context verification in multi-step tasks.
//
// Source records which extractor produced the fact:
//   - "builtin"    — captured by a builtin per-service or generic regex pattern.
//   - "llm_direct" — emitted by the LLM as a "direct fact".
//   - "llm_regex"  — captured by an LLM-emitted regex against the full result.
//   - "unknown"    — legacy rows from before the column was added.
type ChainFact struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id"`
	AuditID   string    `json:"audit_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	FactType  string    `json:"fact_type"`
	FactValue string    `json:"fact_value"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// PairedDevice represents a mobile device paired for push notifications.
type PairedDevice struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	DeviceName       string    `json:"device_name"`
	DeviceToken      string    `json:"-"`
	DeviceHMACKey    string    `json:"-"`
	PushToStartToken string    `json:"-"` // APNs push-to-start token for Live Activities
	PairedAt         time.Time `json:"paired_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// ConnectionRequest represents an agent's request to connect to this daemon.
type ConnectionRequest struct {
	ID             string          `json:"id"`
	UserID         string          `json:"user_id"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	CallbackURL    string          `json:"callback_url,omitempty"`
	Status         string          `json:"status"`             // pending | approved | denied | expired
	AgentID        string          `json:"agent_id,omitempty"` // set on approval
	Token          string          `json:"token,omitempty"`    // raw token, set on approval (never persisted)
	IPAddress      string          `json:"ip_address"`
	CreatedAt      time.Time       `json:"created_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	InstallContext *InstallContext `json:"install_context,omitempty"`
}

// InstallContext captures non-PII facts the installer skill discovered about
// the calling environment. Set at mint time, displayed on the approval card,
// and persisted on the connection request for downstream debugging. Every
// field is optional — the skill sends as much as it knows.
type InstallContext struct {
	Harness        string `json:"harness,omitempty"` // claude-code | codex | hermes | openclaw | claude-desktop
	HarnessVersion string `json:"harness_version,omitempty"`
	InstallMode    string `json:"install_mode,omitempty"` // local | docker | cloud
	HostOS         string `json:"host_os,omitempty"`      // darwin | linux | windows
	ContainerID    string `json:"container_id,omitempty"` // populated when install_mode=docker
	AuthMode       string `json:"auth_mode,omitempty"`    // passthrough | swap
	AliasIntent    string `json:"alias_intent,omitempty"` // none | safe | yolo
}

// OAuthClient is a dynamically registered OAuth 2.1 client (RFC 7591).
type OAuthClient struct {
	ID           string    `json:"client_id"`
	ClientName   string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// GeneratedAdapter is a user-generated adapter YAML definition stored in the database.
type GeneratedAdapter struct {
	UserID      string    `json:"user_id"`
	ServiceID   string    `json:"service_id"`
	YAMLContent string    `json:"yaml_content"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TelegramGroup represents a Telegram group configured for observation.
// Each group has independent auto-approval settings.
type TelegramGroup struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"user_id"`
	GroupChatID         string    `json:"group_chat_id"`
	Title               string    `json:"title"`
	AutoApprovalEnabled bool      `json:"auto_approval_enabled"`
	AutoApprovalNotify  bool      `json:"auto_approval_notify"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// OAuthAuthorizationCode is a one-time-use authorization code for the OAuth 2.1 flow.
type OAuthAuthorizationCode struct {
	CodeHash      string    `json:"-"`
	ClientID      string    `json:"client_id"`
	UserID        string    `json:"user_id"`
	DaemonID      string    `json:"daemon_id,omitempty"` // set when authorized via relay
	RedirectURI   string    `json:"redirect_uri"`
	CodeChallenge string    `json:"code_challenge"`
	Scope         string    `json:"scope"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// FeedbackReport is an agent-submitted bug report about a Clawvisor decision.
type FeedbackReport struct {
	ID          string          `json:"id"`
	UserID      string          `json:"user_id"`
	AgentID     string          `json:"agent_id"`
	AgentName   string          `json:"agent_name"`
	RequestID   string          `json:"request_id,omitempty"` // the gateway request that triggered the report
	TaskID      string          `json:"task_id,omitempty"`    // the task scope at the time
	Category    string          `json:"category"`             // wrong_block | wrong_deny | slow_approval | scope_too_narrow | other
	Description string          `json:"description"`          // free-form agent narrative
	Severity    string          `json:"severity"`             // low | medium | high | critical
	Context     json.RawMessage `json:"context,omitempty"`    // optional structured context the agent provides
	Response    string          `json:"response,omitempty"`   // Clawvisor's response to the agent
	CreatedAt   time.Time       `json:"created_at"`
}

// NPSResponse is a periodic satisfaction score submitted by an agent.
type NPSResponse struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	AgentID   string    `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	TaskID    string    `json:"task_id,omitempty"` // task active when prompted
	Score     int       `json:"score"`             // 1-10
	Feedback  string    `json:"feedback,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// NPSStats holds aggregate NPS data for an agent.
type NPSStats struct {
	TotalResponses int     `json:"total_responses"`
	AverageScore   float64 `json:"average_score"`
	LastScore      int     `json:"last_score"`
	LastFeedback   string  `json:"last_feedback,omitempty"`
}
