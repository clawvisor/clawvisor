package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: record not found")

// ErrConflict is returned when a uniqueness constraint is violated.
var ErrConflict = errors.New("store: record already exists")

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
	GetAgentByToken(ctx context.Context, tokenHash string) (*Agent, error)
	ListAgents(ctx context.Context, userID string) ([]*Agent, error)
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

	// Notification configs
	UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error
	GetNotificationConfig(ctx context.Context, userID, channel string) (*NotificationConfig, error)
	DeleteNotificationConfig(ctx context.Context, userID, channel string) error
	ListNotificationConfigsByChannel(ctx context.Context, channel string) ([]NotificationConfig, error)

	// Gateway request log (append-only backup)
	LogGatewayRequest(ctx context.Context, entry *GatewayRequestLog) error

	// Audit log
	LogAudit(ctx context.Context, entry *AuditEntry) error
	UpdateAuditOutcome(ctx context.Context, id, outcome, errMsg string, durationMS int) error
	GetAuditEntry(ctx context.Context, id, userID string) (*AuditEntry, error)
	GetAuditEntryByRequestID(ctx context.Context, requestID, userID string) (*AuditEntry, error)
	ListAuditEntries(ctx context.Context, userID string, filter AuditFilter) ([]*AuditEntry, int, error)
	AuditActivityBuckets(ctx context.Context, userID string, since time.Time, bucketMinutes int) ([]ActivityBucket, error)

	// Tasks
	CreateTask(ctx context.Context, task *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, userID string, filter TaskFilter) ([]*Task, int, error)
	UpdateTaskStatus(ctx context.Context, id, status string) error
	UpdateTaskApproved(ctx context.Context, id string, expiresAt time.Time) error
	UpdateTaskActions(ctx context.Context, id string, actions []TaskAction, expiresAt time.Time) error
	IncrementTaskRequestCount(ctx context.Context, id string) error
	SetTaskPendingExpansion(ctx context.Context, id string, action *TaskAction, reason string) error
	ListExpiredTasks(ctx context.Context) ([]*Task, error)
	RevokeTask(ctx context.Context, id, userID string) error
	RevokeTasksByAgent(ctx context.Context, agentID, userID string) (int, error)

	// Pending approvals
	SavePendingApproval(ctx context.Context, pa *PendingApproval) error
	GetPendingApproval(ctx context.Context, requestID string) (*PendingApproval, error)
	ListPendingApprovals(ctx context.Context, userID string) ([]*PendingApproval, error)
	DeletePendingApproval(ctx context.Context, requestID string) error
	ListExpiredPendingApprovals(ctx context.Context) ([]*PendingApproval, error)
	UpdatePendingApprovalStatus(ctx context.Context, requestID, status string) error
	// ClaimPendingApprovalForExecution atomically transitions a pending approval
	// from "approved" to "executing". Returns true if the caller won the claim,
	// false if another caller already claimed it (or the row is not "approved").
	ClaimPendingApprovalForExecution(ctx context.Context, requestID string) (bool, error)

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
	DeleteExpiredConnectionRequests(ctx context.Context) error
	CountPendingConnectionRequests(ctx context.Context) (int, error)

	// Bridge tokens (OpenClaw plugin identity — writes to buffer, never to tools)
	CreateBridgeToken(ctx context.Context, bt *BridgeToken) error
	GetBridgeTokenByHash(ctx context.Context, tokenHash string) (*BridgeToken, error)
	GetBridgeTokenByID(ctx context.Context, id string) (*BridgeToken, error)
	ListBridgeTokens(ctx context.Context, userID string) ([]*BridgeToken, error)
	ListActiveBridgesForUser(ctx context.Context, userID string) ([]*BridgeToken, error)
	UpdateBridgeTokenAutoApproval(ctx context.Context, id, userID string, enabled bool) error
	RevokeBridgeToken(ctx context.Context, id, userID string) error
	TouchBridgeTokenLastUsed(ctx context.Context, id string) error
	// AdvanceBridgeLastSeq atomically bumps the bridge's last_seq to
	// newSeq iff last_seq < newSeq. Returns true when advanced; false when
	// newSeq ≤ current last_seq (the caller must reject the ingest as a
	// regression or duplicate-seq-with-different-event).
	AdvanceBridgeLastSeq(ctx context.Context, id string, newSeq int64) (bool, error)

	// Plugin pair requests (long-poll pairing lifecycle for OpenClaw plugin)
	CreatePluginPairRequest(ctx context.Context, req *PluginPairRequest) error
	GetPluginPairRequest(ctx context.Context, id string) (*PluginPairRequest, error)
	GetPluginPairRequestByIdempotencyKey(ctx context.Context, userID, idempotencyKey string) (*PluginPairRequest, error)
	ListPendingPluginPairRequests(ctx context.Context, userID string) ([]*PluginPairRequest, error)
	UpdatePluginPairRequestStatus(ctx context.Context, id, status, bridgeTokenID string) error
	DeleteExpiredPluginPairRequests(ctx context.Context) error
	// ApprovePluginPair performs the "approve" side of pairing as a single
	// DB transaction: mints the bridge_token row (if not already attached),
	// mints one agent row per entry in Agents, and flips the pair request
	// to "approved" — atomically. Prior non-atomic implementation could
	// leave orphan tokens if the final status update failed.
	ApprovePluginPair(ctx context.Context, input ApprovePluginPairInput) (*ApprovePluginPairOutput, error)

	// Plugin pair codes (one-time capabilities minted by the dashboard and
	// redeemed by the plugin to initiate its pair flow)
	CreatePluginPairCode(ctx context.Context, pc *PluginPairCode) error
	GetPluginPairCodeByHash(ctx context.Context, codeHash string) (*PluginPairCode, error)
	ConsumePluginPairCode(ctx context.Context, codeHash string) (*PluginPairCode, error)
	DeleteExpiredPluginPairCodes(ctx context.Context) error

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
	Agents          int            // total registered agents
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
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	Name            string     `json:"name"`
	TokenHash       string     `json:"-"`
	OrgID           string     `json:"org_id,omitempty"` // set by cloud when agent belongs to an org
	CreatedAt       time.Time  `json:"created_at"`
	ActiveTaskCount int        `json:"active_task_count"`
	LastTaskAt      *time.Time `json:"last_task_at,omitempty"`
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
	ID             string          `json:"id"`
	UserID         string          `json:"user_id"`
	AgentID        *string         `json:"agent_id,omitempty"`
	RequestID      string          `json:"request_id"`
	TaskID         *string         `json:"task_id,omitempty"`
	Timestamp      time.Time       `json:"timestamp"`
	Service        string          `json:"service"`
	Action         string          `json:"action"`
	ParamsSafe     json.RawMessage `json:"params_safe"`
	Decision       string          `json:"decision"`
	Outcome        string          `json:"outcome"`
	PolicyID       *string         `json:"policy_id,omitempty"`
	RuleID         *string         `json:"rule_id,omitempty"`
	SafetyFlagged  bool            `json:"safety_flagged"`
	SafetyReason   *string         `json:"safety_reason,omitempty"`
	Reason         *string         `json:"reason,omitempty"`
	DataOrigin     *string         `json:"data_origin,omitempty"`
	ContextSrc     *string         `json:"context_src,omitempty"`
	DurationMS     int             `json:"duration_ms"`
	FiltersApplied json.RawMessage `json:"filters_applied,omitempty"`
	Verification   json.RawMessage `json:"verification,omitempty"`
	ErrorMsg       *string         `json:"error_msg,omitempty"`
}

// TaskAction represents a single authorized action within a task scope.
type TaskAction struct {
	Service            string          `json:"service"`
	Action             string          `json:"action"`          // specific action or "*"
	AutoExecute        bool            `json:"auto_execute"`
	ResponseFilters    json.RawMessage `json:"response_filters,omitempty"`
	ExpectedUse        string          `json:"expected_use,omitempty"`
	ExpansionRationale string          `json:"expansion_rationale,omitempty"` // set from PendingReason when scope expansion is approved
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
	ID                string       `json:"id"`
	UserID            string       `json:"user_id"`
	AgentID           string       `json:"agent_id"`
	Purpose           string       `json:"purpose"`
	Status            string       `json:"status"` // pending_approval | active | completed | expired | denied | cancelled | pending_scope_expansion | revoked
	Lifetime          string       `json:"lifetime"` // session | standing
	AuthorizedActions []TaskAction  `json:"authorized_actions"`
	PlannedCalls      []PlannedCall `json:"planned_calls,omitempty"`
	CallbackURL       *string       `json:"callback_url,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	ApprovedAt        *time.Time   `json:"approved_at,omitempty"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty"`
	ExpiresInSeconds  int          `json:"expires_in_seconds,omitempty"`
	RequestCount      int          `json:"request_count"`
	// PendingAction holds the action awaiting scope expansion approval.
	PendingAction *TaskAction `json:"pending_action,omitempty"`
	PendingReason string      `json:"pending_reason,omitempty"`
	// RiskLevel is the LLM-assessed risk level ("low", "medium", "high", "critical", "unknown", or "").
	RiskLevel   string          `json:"risk_level,omitempty"`
	RiskDetails json.RawMessage `json:"risk_details,omitempty"`
	// ApprovalSource indicates how the task was approved ("", "manual", "telegram_group", "telegram_button").
	ApprovalSource    string          `json:"approval_source,omitempty"`
	ApprovalRationale json.RawMessage `json:"approval_rationale,omitempty"`
}

// PendingApproval is a gateway request awaiting human approval.
type PendingApproval struct {
	ID            string          `json:"id"`
	UserID        string          `json:"user_id"`
	RequestID     string          `json:"request_id"`
	AuditID       string          `json:"audit_id"`
	RequestBlob   json.RawMessage `json:"request_blob"`
	CallbackURL   *string         `json:"callback_url,omitempty"`
	Status        string          `json:"status"` // "pending" or "approved"
	ExpiresAt     time.Time       `json:"expires_at"`
	CreatedAt     time.Time       `json:"created_at"`
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
	Service    string // filter by service
	Outcome    string // filter by outcome
	DataOrigin string // filter by data_origin
	TaskID     string // filter by task_id
	Limit      int    // 0 -> default (50)
	Offset     int
}

// ActivityBucket is one row of the aggregated audit activity histogram.
type ActivityBucket struct {
	Bucket  time.Time `json:"bucket"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}

// ChainFact is a structural reference extracted from an adapter result for
// chain context verification in multi-step tasks.
type ChainFact struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id"`
	AuditID   string    `json:"audit_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	FactType  string    `json:"fact_type"`
	FactValue string    `json:"fact_value"`
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
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CallbackURL string    `json:"callback_url,omitempty"`
	Status      string    `json:"status"`              // pending | approved | denied | expired
	AgentID     string    `json:"agent_id,omitempty"`   // set on approval
	Token       string    `json:"token,omitempty"`      // raw token, set on approval (never persisted)
	IPAddress   string    `json:"ip_address"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// BridgeToken is a long-lived bearer token minted for an OpenClaw plugin
// install. It is scoped to /api/buffer/ingest and a small set of plugin-
// management endpoints — distinct from the agent tokens used for task and
// gateway calls, so the agent (potentially running untrusted LLM output)
// cannot forge or observe it.
type BridgeToken struct {
	ID                  string     `json:"id"`
	UserID              string     `json:"user_id"`
	TokenHash           string     `json:"-"`
	InstallFingerprint  string     `json:"install_fingerprint"`
	Hostname            string     `json:"hostname"`
	AutoApprovalEnabled bool       `json:"auto_approval_enabled"`
	// LastSeq is the high-water mark of ingest events accepted from this
	// bridge. Advanced atomically by AdvanceBridgeLastSeq so racing
	// ingests can't skip or reorder seq.
	LastSeq    int64      `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// PluginPairRequest is the long-poll record for an OpenClaw plugin pairing
// lifecycle. On approve, a BridgeToken row is minted (id stored here) and
// one Agent row per entry in AgentIDs.
type PluginPairRequest struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"user_id"`
	InstallFingerprint string    `json:"install_fingerprint"`
	Hostname           string    `json:"hostname"`
	AgentIDs           []string  `json:"agent_ids"`
	Status             string    `json:"status"` // pending | approved | denied | expired
	BridgeTokenID      string    `json:"bridge_token_id,omitempty"`
	IdempotencyKey     string    `json:"-"` // not exposed externally; used to collapse retries
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// ApprovePluginPairInput bundles everything the store needs to commit a
// plugin-pair approval in a single transaction. The handler generates raw
// tokens (bridge + per-agent), hashes them, and packages hashes here;
// raw values never touch the DB layer.
type ApprovePluginPairInput struct {
	PairRequestID string
	UserID        string // must match pair_request.user_id
	// NewBridge is non-nil for initial pairs (no bridge_token_id yet on
	// the pair request). Nil for agent-add requests that reuse an
	// existing bridge — the transaction will flip the pair request to
	// approved and mint only the agent rows.
	NewBridge *NewBridgeInput
	// Agents is the set of per-agent records to create in this
	// transaction, keyed by the caller-supplied agent name.
	Agents []AgentMintInput
}

// NewBridgeInput is the bridge_token payload minted during initial pair.
type NewBridgeInput struct {
	TokenHash           string
	InstallFingerprint  string
	Hostname            string
	AutoApprovalEnabled bool
}

// AgentMintInput is a single agent row to create during pair approval.
type AgentMintInput struct {
	Name      string // human-facing agent name (e.g. "main", "researcher")
	TokenHash string
}

// ApprovePluginPairOutput is returned to the handler after a successful
// transactional approve. The handler uses BridgeTokenID to populate the
// token bundle cache; AgentIDs is the name→uuid mapping for diagnostics.
type ApprovePluginPairOutput struct {
	BridgeTokenID string
	AgentIDs      map[string]string // agent name → agent uuid
}

// PluginPairCode is a one-time capability minted by the dashboard and
// redeemed by the OpenClaw plugin to initiate its pair flow. Short-lived,
// single-use, SHA-256-hashed at rest.
type PluginPairCode struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	CodeHash   string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ConsumedAt *time.Time `json:"consumed_at,omitempty"`
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
	RequestID   string          `json:"request_id,omitempty"`   // the gateway request that triggered the report
	TaskID      string          `json:"task_id,omitempty"`      // the task scope at the time
	Category    string          `json:"category"`               // wrong_block | wrong_deny | slow_approval | scope_too_narrow | other
	Description string          `json:"description"`            // free-form agent narrative
	Severity    string          `json:"severity"`               // low | medium | high | critical
	Context     json.RawMessage `json:"context,omitempty"`      // optional structured context the agent provides
	Response    string          `json:"response,omitempty"`     // Clawvisor's response to the agent
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
