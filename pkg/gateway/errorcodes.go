package gateway

// Error codes returned to gateway clients in the "code" field of error/non-OK
// responses. Clients should switch on these codes rather than parsing the
// human-readable "error" or "reason" prose, which may change without notice.
//
// Codes are grouped by category. Adding a new code is backwards-compatible;
// renaming or removing a code is not.
const (
	// Request validation — the request was malformed or missing fields.
	CodeInvalidRequest     = "INVALID_REQUEST"
	CodeInvalidParams      = "INVALID_PARAMS"
	CodeInvalidCallbackURL = "INVALID_CALLBACK_URL"
	CodeMissingReason      = "MISSING_REASON"
	CodeMissingSessionID   = "MISSING_SESSION_ID"
	CodeTaskRequired       = "TASK_REQUIRED"

	// Auth / ownership.
	CodeUnauthorized = "UNAUTHORIZED"
	CodeForbidden    = "FORBIDDEN"

	// Addressing — the named service or action does not exist.
	CodeUnknownService = "UNKNOWN_SERVICE"
	CodeUnknownAction  = "UNKNOWN_ACTION"

	// State — the target task/approval is not in a state that permits this action.
	CodeInvalidState      = "INVALID_STATE"
	CodeNotFound          = "NOT_FOUND"
	CodeAlreadyExecuting  = "ALREADY_EXECUTING"
	CodeNotApproved       = "NOT_APPROVED"
	CodeApprovalExpired   = "APPROVAL_EXPIRED"

	// Policy — the request is syntactically fine but blocked by authorization.
	CodeScopeMismatch  = "SCOPE_MISMATCH"  // outside the approved task scope
	CodeReasonTooVague = "REASON_TOO_VAGUE" // intent verifier found reason incoherent/insufficient
	CodeParamViolation = "PARAM_VIOLATION"  // params inconsistent with task scope / chain context
	CodeRestricted     = "RESTRICTED"       // user restriction or org policy blocked the action
	CodeMissingScopes  = "MISSING_SCOPES"   // activation is missing OAuth scopes required for this action

	// Execution — downstream failure during or after dispatch.
	CodeAdapterError            = "ADAPTER_ERROR"
	CodeLocalServiceUnavailable = "LOCAL_SERVICE_UNAVAILABLE"

	// Operational.
	CodeRateLimited  = "RATE_LIMITED"
	CodeInternalError = "INTERNAL_ERROR"

	// Batch — aggregate endpoint specific.
	CodeBatchTooLarge = "BATCH_TOO_LARGE"
	CodeBatchEmpty    = "BATCH_EMPTY"
)
