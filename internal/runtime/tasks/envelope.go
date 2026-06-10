package tasks

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type ExpectedTool struct {
	ToolName   string         `json:"tool_name"`
	Why        string         `json:"why"`
	InputShape map[string]any `json:"input_shape,omitempty"`
	InputRegex string         `json:"input_regex,omitempty"`
}

type ExpectedEgress struct {
	Host            string         `json:"host"`
	Why             string         `json:"why"`
	Method          string         `json:"method,omitempty"`
	Path            string         `json:"path,omitempty"`
	PathRegex       string         `json:"path_regex,omitempty"`
	QueryShape      map[string]any `json:"query_shape,omitempty"`
	BodyShape       map[string]any `json:"body_shape,omitempty"`
	Headers         map[string]any `json:"headers,omitempty"`
	CredentialAlias string         `json:"credential_alias,omitempty"`
}

type RequiredCredential struct {
	VaultItemID     string `json:"vault_item_id,omitempty"`
	VaultItemHandle string `json:"vault_item_handle,omitempty"`
	Why             string `json:"why"`
}

type Envelope struct {
	ExpectedTools          []ExpectedTool
	ExpectedEgress         []ExpectedEgress
	RequiredCredentials    []RequiredCredential
	IntentVerificationMode string
	ExpectedUse            string
	SchemaVersion          int
}

// TaskCreateRequest is the parsed body of `POST /api/control/tasks` (or
// equivalently `POST /api/tasks`). The full validating handler lives in
// internal/api/handlers; this lighter shape is used by the lite-proxy's
// inline task-approval flow to inspect a model-emitted task definition
// and (on approval) hand the same payload back to the task-creation
// helper.
//
// Field tags match the wire format. The runtime/tasks package lives
// outside internal/api/handlers to avoid an import cycle between the
// llm-proxy and the handlers package.
type TaskCreateRequest struct {
	Purpose                string               `json:"purpose"`
	AuthorizedActions      []map[string]any     `json:"authorized_actions,omitempty"`
	PlannedCalls           []map[string]any     `json:"planned_calls,omitempty"`
	ExpectedTools          []ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress         []ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials    []RequiredCredential `json:"required_credentials,omitempty"`
	IntentVerificationMode string               `json:"intent_verification_mode,omitempty"`
	ExpectedUse            string               `json:"expected_use,omitempty"`
	SchemaVersion          int                  `json:"schema_version,omitempty"`
	ExpiresInSeconds       int                  `json:"expires_in_seconds,omitempty"`
	CallbackURL            string               `json:"callback_url,omitempty"`
	Lifetime               string               `json:"lifetime,omitempty"`
}

// PendingExpansion is the in-flight expansion request awaiting user
// approval: the additions the agent proposed (in the same shape as the
// runtime envelope) plus the one-line reason the model gave. It is the
// short-lived state persisted while the user decides.
//
// Storing the full envelope rather than a single (service, action) lets
// expansion mirror task creation: the agent declares expected_tools /
// expected_egress / required_credentials with per-item `why`, and the
// user approves (or denies) the full delta at once.
type PendingExpansion struct {
	ExpectedTools       []ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress      []ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials []RequiredCredential `json:"required_credentials,omitempty"`
	Reason              string               `json:"reason,omitempty"`
}

// ReplacedExpectedTool captures both the prior and replacement entry
// for a tool whose `why` was overwritten during envelope merge. The
// approval prompt needs the prior `why` (for "was: …") AND the new
// `why` (for "now: …") so the reviewer sees the actual scope change.
type ReplacedExpectedTool struct {
	Prior ExpectedTool
	New   ExpectedTool
}

// ReplacedExpectedEgress mirrors ReplacedExpectedTool for egress
// entries.
type ReplacedExpectedEgress struct {
	Prior ExpectedEgress
	New   ExpectedEgress
}

// ReplacedRequiredCredential mirrors ReplacedExpectedTool for
// credential entries.
type ReplacedRequiredCredential struct {
	Prior RequiredCredential
	New   RequiredCredential
}

// EnvelopeMergeResult describes the outcome of merging an expansion
// envelope into a parent task's envelope. `Merged` is the envelope to
// persist on approval. The Added/Replaced slices feed the approval
// prompt renderer so the user sees what is genuinely new versus what is
// having its `why` revised — Replaced entries carry both the prior and
// new `why` so the renderer can show a was/now diff.
type EnvelopeMergeResult struct {
	Merged              Envelope
	AddedTools          []ExpectedTool
	ReplacedTools       []ReplacedExpectedTool
	AddedEgress         []ExpectedEgress
	ReplacedEgress      []ReplacedExpectedEgress
	AddedCredentials    []RequiredCredential
	ReplacedCredentials []ReplacedRequiredCredential
}

// MergeEnvelopes folds an expansion envelope into a parent envelope
// using replace-by-name dedup. For each addition whose canonical key
// (lowercased tool_name / host / vault_item_id-or-handle) matches an
// existing entry, the new `why` wholesale replaces the old one — no
// merge, no append. Genuinely new keys are appended.
//
// The contract forces the agent to write a single coherent `why` that
// subsumes both old and new purposes if it wants continuity, rather
// than letting "and also X" sprawl accumulate in the audit trail.
//
// Non-`why` fields on a replaced entry (InputShape, Method, etc.) are
// taken from the addition. The expectation is that an expansion
// effectively redefines the entry; a partial expansion that wants to
// preserve an existing field shape should restate it.
func MergeEnvelopes(parent, additions Envelope) EnvelopeMergeResult {
	out := EnvelopeMergeResult{
		Merged: Envelope{
			IntentVerificationMode: parent.IntentVerificationMode,
			ExpectedUse:            parent.ExpectedUse,
			SchemaVersion:          parent.SchemaVersion,
		},
	}
	if out.Merged.SchemaVersion == 0 {
		out.Merged.SchemaVersion = additions.SchemaVersion
	}

	out.Merged.ExpectedTools, out.AddedTools, out.ReplacedTools =
		mergeExpectedTools(parent.ExpectedTools, additions.ExpectedTools)
	out.Merged.ExpectedEgress, out.AddedEgress, out.ReplacedEgress =
		mergeExpectedEgress(parent.ExpectedEgress, additions.ExpectedEgress)
	out.Merged.RequiredCredentials, out.AddedCredentials, out.ReplacedCredentials =
		mergeRequiredCredentials(parent.RequiredCredentials, additions.RequiredCredentials)
	return out
}

func mergeExpectedTools(parent, additions []ExpectedTool) (merged, added []ExpectedTool, replaced []ReplacedExpectedTool) {
	if len(additions) == 0 {
		return slices.Clone(parent), nil, nil
	}
	index := make(map[string]int, len(parent))
	merged = slices.Clone(parent)
	for i, item := range parent {
		key := expectedToolKey(item)
		if key == "" {
			continue
		}
		index[key] = i
	}
	for _, item := range additions {
		key := expectedToolKey(item)
		if key == "" {
			// Skip empty-named entries rather than appending garbage;
			// validation upstream (ValidateTaskEnvelope) rejects them
			// before we get here, but keeping the merger total avoids
			// surprise if a caller skips validation.
			continue
		}
		if idx, ok := index[key]; ok {
			// Replace-by-name is a *why* update, not a tool rename or
			// shape change. Preserve:
			//   - the parent's identifier casing (so a case-mismatched
			//     addition doesn't render as a rename in the approval
			//     prompt — `Bash` vs `bash`)
			//   - InputShape / InputRegex (silently relaxing these
			//     would widen the previously approved tool's shape
			//     without the reviewer seeing the change — the prompt
			//     only diffs `why`).
			// Agents that genuinely need to change shape must create a
			// new task or use the dashboard's scope-overrides surface.
			prior := merged[idx]
			merged[idx] = ExpectedTool{
				ToolName:   prior.ToolName,
				Why:        item.Why,
				InputShape: prior.InputShape,
				InputRegex: prior.InputRegex,
			}
			replaced = append(replaced, ReplacedExpectedTool{Prior: prior, New: merged[idx]})
			continue
		}
		index[key] = len(merged)
		merged = append(merged, item)
		added = append(added, item)
	}
	return merged, added, replaced
}

func mergeExpectedEgress(parent, additions []ExpectedEgress) (merged, added []ExpectedEgress, replaced []ReplacedExpectedEgress) {
	if len(additions) == 0 {
		return slices.Clone(parent), nil, nil
	}
	index := make(map[string]int, len(parent))
	merged = slices.Clone(parent)
	for i, item := range parent {
		key := expectedEgressKey(item)
		if key == "" {
			continue
		}
		index[key] = i
	}
	for _, item := range additions {
		key := expectedEgressKey(item)
		if key == "" {
			continue
		}
		if idx, ok := index[key]; ok {
			// Replace-by-name is a *why* update only. Preserve every
			// structural field from the parent — Method, Path,
			// PathRegex, QueryShape, BodyShape, Headers,
			// CredentialAlias, and the host casing — so an addition
			// that names the same host but changes (say) Method from
			// GET to POST does NOT silently narrow the parent's
			// already-approved scope. The approval prompt diffs only
			// `why`; if structural fields could change here, the
			// reviewer would have no signal that the gateway's
			// per-call matcher (bestEgressMatchInTask) is about to
			// reject calls the parent task previously authorized.
			prior := merged[idx]
			merged[idx] = ExpectedEgress{
				Host:            prior.Host,
				Why:             item.Why,
				Method:          prior.Method,
				Path:            prior.Path,
				PathRegex:       prior.PathRegex,
				QueryShape:      prior.QueryShape,
				BodyShape:       prior.BodyShape,
				Headers:         prior.Headers,
				CredentialAlias: prior.CredentialAlias,
			}
			replaced = append(replaced, ReplacedExpectedEgress{Prior: prior, New: merged[idx]})
			continue
		}
		index[key] = len(merged)
		merged = append(merged, item)
		added = append(added, item)
	}
	return merged, added, replaced
}

func mergeRequiredCredentials(parent, additions []RequiredCredential) (merged, added []RequiredCredential, replaced []ReplacedRequiredCredential) {
	if len(additions) == 0 {
		return slices.Clone(parent), nil, nil
	}
	index := make(map[string]int, len(parent))
	merged = slices.Clone(parent)
	for i, item := range parent {
		key := requiredCredentialKey(item)
		if key == "" {
			continue
		}
		index[key] = i
	}
	for _, item := range additions {
		key := requiredCredentialKey(item)
		if key == "" {
			continue
		}
		if idx, ok := index[key]; ok {
			replaced = append(replaced, ReplacedRequiredCredential{Prior: merged[idx], New: item})
			merged[idx] = item
			continue
		}
		index[key] = len(merged)
		merged = append(merged, item)
		added = append(added, item)
	}
	return merged, added, replaced
}

// expectedToolKey is the canonical dedup key for a tool entry. Tool
// names are case-insensitive on disk in practice (harnesses normalize
// them inconsistently), so the key lowercases. Empty names produce ""
// which the merger treats as unindexed.
func expectedToolKey(t ExpectedTool) string {
	return strings.ToLower(strings.TrimSpace(t.ToolName))
}

// expectedEgressKey is the canonical dedup key for an egress entry.
// Hosts are case-insensitive per RFC 3986; we lowercase to match. Two
// egress entries to the same host but different paths still collide
// here because the agent's `why` is per-host in practice and a per-
// path dedup would let an agent quietly widen scope by appending a new
// path entry alongside the existing host.
func expectedEgressKey(e ExpectedEgress) string {
	return strings.ToLower(strings.TrimSpace(e.Host))
}

// requiredCredentialKey is the canonical dedup key for a credential
// entry. vault_item_id and vault_item_handle name credentials through
// different identifier kinds; the canonical key includes the kind
// prefix so they cannot collide. If they collided on the lowercased
// value, replace-by-name would swap which identifier field carries the
// value — leaving the merged entry with only one of the two populated
// and the downstream lookup picking the wrong resolution path.
//
// Entries with both fields populated (which validation already rejects)
// fall through to the id-kind key so the merger remains total.
func requiredCredentialKey(c RequiredCredential) string {
	if id := strings.ToLower(strings.TrimSpace(c.VaultItemID)); id != "" {
		return "id:" + id
	}
	if handle := strings.ToLower(strings.TrimSpace(c.VaultItemHandle)); handle != "" {
		return "handle:" + handle
	}
	return ""
}

func EnvelopeFromTask(task *store.Task) (Envelope, error) {
	env := Envelope{
		IntentVerificationMode: task.IntentVerificationMode,
		ExpectedUse:            task.ExpectedUse,
		SchemaVersion:          task.SchemaVersion,
	}
	if task.SchemaVersion == 0 {
		env.SchemaVersion = 1
	}
	if len(task.ExpectedTools) > 0 {
		if err := json.Unmarshal(task.ExpectedTools, &env.ExpectedTools); err != nil {
			return Envelope{}, err
		}
	}
	if len(task.ExpectedEgress) > 0 {
		if err := json.Unmarshal(task.ExpectedEgress, &env.ExpectedEgress); err != nil {
			return Envelope{}, err
		}
	}
	if len(task.RequiredCredentials) > 0 {
		if err := json.Unmarshal(task.RequiredCredentials, &env.RequiredCredentials); err != nil {
			return Envelope{}, err
		}
	}
	return env, nil
}
