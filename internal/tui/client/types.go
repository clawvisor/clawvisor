package client

import "time"

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
	PendingAction     *TaskAction  `json:"pending_action,omitempty"`
	PendingReason     string       `json:"pending_reason,omitempty"`
}

type TaskAction struct {
	Service     string `json:"service"`
	Action      string `json:"action"`
	AutoExecute bool   `json:"auto_execute"`
	ExpectedUse string `json:"expected_use,omitempty"`
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
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Alias              string   `json:"alias,omitempty"`
	OAuth              bool     `json:"oauth"`
	RequiresActivation bool     `json:"requires_activation"`
	Actions            []string `json:"actions"`
	Status             string   `json:"status"` // "activated" or "not_activated"
	ActivatedAt        string   `json:"activated_at,omitempty"`
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
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Token     string    `json:"token,omitempty"` // only on creation
}
