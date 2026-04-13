package client

import (
	"encoding/json"
	"time"
)

// ── Public Config ────────────────────────────────────────────────────────────

type PublicConfig struct {
	AuthMode string `json:"auth_mode"`
}

// ── Auth ────────────────────────────────────────────────────────────────────

type AuthResponse struct {
	User         User   `json:"user"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// ── Queue ───────────────────────────────────────────────────────────────────

type QueueResponse struct {
	Items []QueueItem `json:"items"`
	Total int         `json:"total"`
}

type QueueItem struct {
	Type      string          `json:"type"` // "approval" or "task"
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt *time.Time      `json:"expires_at"`
	Approval  *QueueApproval  `json:"approval,omitempty"`
	Task      *Task           `json:"task,omitempty"`
}

type QueueApproval struct {
	RequestID string                 `json:"request_id"`
	AuditID   string                 `json:"audit_id"`
	Service   string                 `json:"service"`
	Action    string                 `json:"action"`
	Params    map[string]interface{} `json:"params"`
	Reason    string                 `json:"reason"`
}

// ── Tasks ───────────────────────────────────────────────────────────────────

type TasksResponse struct {
	Tasks []Task `json:"tasks"`
	Total int    `json:"total"`
}

type Task struct {
	ID                string       `json:"id"`
	UserID            string       `json:"user_id"`
	AgentID           string       `json:"agent_id"`
	AgentName         string       `json:"agent_name,omitempty"`
	Purpose           string       `json:"purpose"`
	Lifetime          string       `json:"lifetime"` // "session" or "standing"
	Status            string       `json:"status"`
	AuthorizedActions []TaskAction `json:"authorized_actions"`
	CallbackURL       string       `json:"callback_url,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	ApprovedAt        *time.Time   `json:"approved_at,omitempty"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty"`
	ExpiresInSeconds  int          `json:"expires_in_seconds"`
	RequestCount      int          `json:"request_count"`
	PendingAction     *TaskAction     `json:"pending_action,omitempty"`
	PendingReason     string          `json:"pending_reason,omitempty"`
	RiskLevel         string          `json:"risk_level,omitempty"`
	RiskDetails       json.RawMessage `json:"risk_details,omitempty"`
}

type TaskAction struct {
	Service     string `json:"service"`
	Action      string `json:"action"`
	AutoExecute bool   `json:"auto_execute"`
	ExpectedUse string `json:"expected_use,omitempty"`
}

type RiskAssessment struct {
	RiskLevel   string         `json:"risk_level"`
	Explanation string         `json:"explanation"`
	Factors     []string       `json:"factors"`
	Conflicts   []RiskConflict `json:"conflicts"`
	Model       string         `json:"model"`
	LatencyMs   int            `json:"latency_ms"`
}

type RiskConflict struct {
	Field       string `json:"field"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
}

type TaskActionResponse struct {
	TaskID    string     `json:"task_id"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ── Approvals ───────────────────────────────────────────────────────────────

type ApprovalsResponse struct {
	Entries []PendingApproval `json:"entries"`
	Total   int               `json:"total"`
}

type PendingApproval struct {
	ID          string      `json:"id"`
	UserID      string      `json:"user_id"`
	RequestID   string      `json:"request_id"`
	AuditID     string      `json:"audit_id"`
	RequestBlob RequestBlob `json:"request_blob"`
	ExpiresAt   time.Time   `json:"expires_at"`
	CreatedAt   time.Time   `json:"created_at"`
}

type RequestBlob struct {
	Service     string                 `json:"service"`
	Action      string                 `json:"action"`
	Params      map[string]interface{} `json:"params"`
	Reason      string                 `json:"reason"`
	CallbackURL string                 `json:"callback_url"`
}

type ApprovalActionResponse struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	AuditID   string `json:"audit_id"`
}

// ── Audit ───────────────────────────────────────────────────────────────────

type AuditResponse struct {
	Entries []AuditEntry `json:"entries"`
	Total   int          `json:"total"`
}

type AuditEntry struct {
	ID            string                 `json:"id"`
	UserID        string                 `json:"user_id"`
	AgentID       string                 `json:"agent_id,omitempty"`
	RequestID     string                 `json:"request_id"`
	TaskID        string                 `json:"task_id,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Service       string                 `json:"service"`
	Action        string                 `json:"action"`
	ParamsSafe    map[string]interface{} `json:"params_safe"`
	Decision      string                 `json:"decision"`
	Outcome       string                 `json:"outcome"`
	SafetyFlagged bool                   `json:"safety_flagged"`
	SafetyReason  string                 `json:"safety_reason,omitempty"`
	Reason        string                 `json:"reason,omitempty"`
	DataOrigin    string                 `json:"data_origin,omitempty"`
	DurationMs    int                    `json:"duration_ms"`
	Verification  *Verification          `json:"verification,omitempty"`
	ErrorMsg      string                 `json:"error_msg,omitempty"`
}

type Verification struct {
	Allow           bool   `json:"allow"`
	ParamScope      string `json:"param_scope"`
	ReasonCoherence string `json:"reason_coherence"`
	Explanation     string `json:"explanation"`
	Model           string `json:"model"`
	LatencyMs       int    `json:"latency_ms"`
	Cached          bool   `json:"cached"`
}

// ── Services ────────────────────────────────────────────────────────────────

type ServicesResponse struct {
	Services []ServiceInfo `json:"services"`
}

type ServiceInfo struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	Alias              string            `json:"alias,omitempty"`
	OAuth              bool              `json:"oauth"`
	OAuthEndpoint      string            `json:"oauth_endpoint,omitempty"`
	DeviceFlow         bool              `json:"device_flow,omitempty"`
	PKCEFlow           bool              `json:"pkce_flow,omitempty"`
	RequiresActivation bool              `json:"requires_activation"`
	CredentialFree     bool              `json:"credential_free"`
	Actions            json.RawMessage   `json:"actions"`
	Variables          []VariableMeta    `json:"variables,omitempty"`
	Status             string            `json:"status"` // "activated" or "not_activated"
	ActivatedAt        string            `json:"activated_at,omitempty"`
	SetupURL           string            `json:"setup_url,omitempty"`
	KeyHint            string            `json:"key_hint,omitempty"`
}

// VariableMeta holds metadata for a user-configurable adapter variable.
type VariableMeta struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// DeviceFlowStartResponse is returned by DeviceFlowStart.
type DeviceFlowStartResponse struct {
	FlowID          string `json:"flow_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// PKCEFlowStartResponse is returned by PKCEFlowStart.
type PKCEFlowStartResponse struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

// DeviceFlowPollResponse is returned by DeviceFlowPoll.
type DeviceFlowPollResponse struct {
	Status   string `json:"status"` // "pending", "slow_down", "expired", "denied", "complete", "error"
	Interval int    `json:"interval,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ActionDisplayNames parses the Actions field (which may be []string or []object)
// and returns human-readable display names.
func (s ServiceInfo) ActionDisplayNames() []string {
	if len(s.Actions) == 0 {
		return nil
	}
	// Try new format: [{id, display_name, ...}, ...]
	var rich []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(s.Actions, &rich); err == nil && len(rich) > 0 {
		if rich[0].ID != "" { // confirms it's the rich format
			names := make([]string, len(rich))
			for i, r := range rich {
				if r.DisplayName != "" {
					names[i] = r.DisplayName
				} else {
					names[i] = r.ID
				}
			}
			return names
		}
	}
	// Fall back to legacy format: ["action_id", ...]
	var plain []string
	if err := json.Unmarshal(s.Actions, &plain); err == nil {
		return plain
	}
	return nil
}

// ── OAuth URL ───────────────────────────────────────────────────────────────

type OAuthURLResponse struct {
	URL               string `json:"url,omitempty"`
	AlreadyAuthorized bool   `json:"already_authorized,omitempty"`
	Service           string `json:"service,omitempty"`
}

// ── Restrictions ────────────────────────────────────────────────────────────

type Restriction struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ── Overview ────────────────────────────────────────────────────────────────

type OverviewResponse struct {
	Queue       []QueueItem      `json:"queue"`
	QueueTotal  int              `json:"queue_total"`
	ActiveTasks []*Task          `json:"active_tasks"`
	Activity    []ActivityBucket `json:"activity"`
}

type ActivityBucket struct {
	Bucket  time.Time `json:"bucket"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}

// ── Agents ──────────────────────────────────────────────────────────────────

type Agent struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"created_at"`
	Token          string    `json:"token,omitempty"`           // only on creation
	CallbackSecret string    `json:"callback_secret,omitempty"` // only on creation with callback
}

// ── Devices ─────────────────────────────────────────────────────────────────

type StartPairingResponse struct {
	PairingToken string    `json:"pairing_token"`
	Code         string    `json:"code"`
	PairingURL   string    `json:"pairing_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type PairingCodeResponse struct {
	DaemonID  string `json:"daemon_id"`
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"`
}

type PairedDevice struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	PairedAt   time.Time `json:"paired_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// ── Version ─────────────────────────────────────────────────────────────────

type VersionInfo struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	UpdateAvail bool   `json:"update_available"`
	ReleaseURL  string `json:"release_url,omitempty"`
	UpgradeCmd  string `json:"upgrade_command,omitempty"`
}
