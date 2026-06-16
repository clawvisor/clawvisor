package tasks

import (
	"encoding/json"
	"reflect"
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
	// DriftID links this task definition back to a scope-drift menu the
	// agent just resolved. Empty when the agent created the task
	// independently (e.g., proactively at session start). When set and
	// the inline-approval flow lands the corresponding hold, the
	// resolver calls ScopeDriftRegistry.SetOutcome so the agent's retry
	// of the originally-blocked tool_use consumes the one-shot pre-clear.
	DriftID string `json:"drift_id,omitempty"`
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

// MergeEnvelopes folds an expansion envelope into a parent envelope.
//
// Dedup rules:
//   - "Identifier match" — same lowercased tool_name / host / vault
//     id-or-handle (kind-scoped).
//   - "Structurally compatible" — the addition's non-empty structural
//     fields (InputShape/InputRegex for tools; Method/Path/PathRegex/
//     QueryShape/BodyShape/Headers/CredentialAlias for egress) all
//     match the parent entry's. An addition that leaves a structural
//     field empty does not constrain it; an addition that sets it to a
//     different value names a different endpoint.
//   - On an identifier+structural match: the addition is a "why
//     update". The new `why` replaces the old; the parent's identifier
//     casing and structural fields are preserved. This protects the
//     approved scope shape from silent narrowing/widening (the
//     approval prompts only diff `why`).
//   - On an identifier match with structural mismatch: the addition
//     names a different endpoint under the same host/tool. It is
//     APPENDED as a new entry. Both rows coexist in the merged
//     envelope; the reviewer sees both in the approval prompt; the
//     gateway matcher honors both.
//   - On no identifier match: appended.
//
// Credentials have no structural fields beyond the identifier kind, so
// the merge is always a why-update on identifier match. The parent's
// identifier casing is preserved (mirroring the tools/egress rule) so
// a case-mismatched re-statement doesn't silently change the vault
// lookup target.
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
	// index maps each identifier key to ALL parent indices with that
	// key. After the structural-collision fix, two parent entries can
	// share a tool_name when their InputShape/InputRegex differ —
	// they're separate endpoints in the same harness tool.
	index := make(map[string][]int, len(parent))
	merged = slices.Clone(parent)
	for i, item := range parent {
		key := expectedToolKey(item)
		if key == "" {
			continue
		}
		index[key] = append(index[key], i)
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
		matched := -1
		for _, idx := range index[key] {
			if expectedToolStructurallyMatches(merged[idx], item) {
				matched = idx
				break
			}
		}
		if matched >= 0 {
			// Why-update: preserve the parent's identifier casing and
			// structural fields. Silently relaxing InputShape /
			// InputRegex would widen the previously approved tool's
			// shape without the reviewer seeing the change.
			prior := merged[matched]
			merged[matched] = ExpectedTool{
				ToolName:   prior.ToolName,
				Why:        item.Why,
				InputShape: prior.InputShape,
				InputRegex: prior.InputRegex,
			}
			replaced = append(replaced, ReplacedExpectedTool{Prior: prior, New: merged[matched]})
			continue
		}
		// Structurally distinct from every parent entry sharing the
		// same tool_name → append as a new row. The reviewer sees the
		// addition's full shape in the approval prompt, and the
		// gateway matcher honors it on call.
		index[key] = append(index[key], len(merged))
		merged = append(merged, item)
		added = append(added, item)
	}
	return merged, added, replaced
}

func mergeExpectedEgress(parent, additions []ExpectedEgress) (merged, added []ExpectedEgress, replaced []ReplacedExpectedEgress) {
	if len(additions) == 0 {
		return slices.Clone(parent), nil, nil
	}
	index := make(map[string][]int, len(parent))
	merged = slices.Clone(parent)
	for i, item := range parent {
		key := expectedEgressKey(item)
		if key == "" {
			continue
		}
		index[key] = append(index[key], i)
	}
	for _, item := range additions {
		key := expectedEgressKey(item)
		if key == "" {
			continue
		}
		matched := -1
		for _, idx := range index[key] {
			if expectedEgressStructurallyMatches(merged[idx], item) {
				matched = idx
				break
			}
		}
		if matched >= 0 {
			// Why-update: preserve the parent's identifier casing and
			// every structural field. Silently narrowing or widening
			// the structural shape would land invisibly in the
			// approval prompt (which diffs only `why`).
			prior := merged[matched]
			merged[matched] = ExpectedEgress{
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
			replaced = append(replaced, ReplacedExpectedEgress{Prior: prior, New: merged[matched]})
			continue
		}
		// Same host, different structural shape (e.g. POST /orgs vs
		// parent's GET /user/repos): treat as a new endpoint. Without
		// this, the approval prompt would show the addition's shape
		// while persistence kept the parent's — the reviewer would
		// approve a delta that never actually lands.
		index[key] = append(index[key], len(merged))
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
			// Preserve the parent's identifier casing. Credentials are
			// case-sensitive in vault lookup, so a case-mismatched
			// re-statement (e.g. addition writes `GITHUB:FOO` over a
			// parent `github:foo` on the same lowercased key) must not
			// re-stamp the identifier — only the `why` updates. Matches
			// the tools/egress contract.
			prior := merged[idx]
			merged[idx] = RequiredCredential{
				VaultItemID:     prior.VaultItemID,
				VaultItemHandle: prior.VaultItemHandle,
				Why:             item.Why,
			}
			replaced = append(replaced, ReplacedRequiredCredential{Prior: prior, New: merged[idx]})
			continue
		}
		index[key] = len(merged)
		merged = append(merged, item)
		added = append(added, item)
	}
	return merged, added, replaced
}

// expectedToolStructurallyMatches reports whether an addition tool is
// a "why update" for a parent tool entry — i.e. each of the addition's
// non-empty structural fields equals the parent's. An addition that
// leaves a field empty does not constrain it; one that sets it to a
// non-empty different value names a different endpoint.
func expectedToolStructurallyMatches(parent, addition ExpectedTool) bool {
	if addition.InputRegex != "" && addition.InputRegex != parent.InputRegex {
		return false
	}
	if addition.InputShape != nil && !reflect.DeepEqual(addition.InputShape, parent.InputShape) {
		return false
	}
	return true
}

// expectedEgressStructurallyMatches mirrors expectedToolStructurallyMatches
// for egress entries. Method comparison is case-insensitive; all other
// structural fields use exact / deep equality.
func expectedEgressStructurallyMatches(parent, addition ExpectedEgress) bool {
	if addition.Method != "" && !strings.EqualFold(addition.Method, parent.Method) {
		return false
	}
	if addition.Path != "" && addition.Path != parent.Path {
		return false
	}
	if addition.PathRegex != "" && addition.PathRegex != parent.PathRegex {
		return false
	}
	if addition.CredentialAlias != "" && addition.CredentialAlias != parent.CredentialAlias {
		return false
	}
	if addition.QueryShape != nil && !reflect.DeepEqual(addition.QueryShape, parent.QueryShape) {
		return false
	}
	if addition.BodyShape != nil && !reflect.DeepEqual(addition.BodyShape, parent.BodyShape) {
		return false
	}
	if addition.Headers != nil && !reflect.DeepEqual(addition.Headers, parent.Headers) {
		return false
	}
	return true
}

// expectedToolKey is the canonical dedup key for a tool entry. Tool
// names are case-insensitive on disk in practice (harnesses normalize
// them inconsistently), so the key lowercases. Empty names produce ""
// which the merger treats as unindexed.
func expectedToolKey(t ExpectedTool) string {
	return strings.ToLower(strings.TrimSpace(t.ToolName))
}

// expectedEgressKey is the canonical dedup key for an egress entry.
// Hosts are case-insensitive per RFC 3986; we lowercase to match.
// Two egress entries to the same host but different structural shapes
// (Method, Path, PathRegex, …) do NOT collide here — they're separate
// endpoints under the same host. See expectedEgressStructurallyMatches
// for the structural compatibility check applied at merge time.
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

// PendingFromAdditions encodes an additions envelope into the
// store.PendingTaskExpansion shape (each envelope array marshalled
// individually as raw JSON, plus the agent's reason). Symmetric to
// the handler's pendingExpansionToEnvelope decode and shares the same
// per-field marshal pattern as EnvelopeToRawColumns. Returns nil
// pending when there are no additions and no reason — callers gate
// that case before reaching here, but the helper stays total.
//
// Lives next to EnvelopeFromTask/EnvelopeToRawColumns so the four
// envelope-encode call sites in handlers go through one tested code
// path.
func PendingFromAdditions(additions Envelope, reason string) (*store.PendingTaskExpansion, error) {
	toolsRaw, egressRaw, credsRaw, err := EnvelopeToRawColumns(additions)
	if err != nil {
		return nil, err
	}
	return &store.PendingTaskExpansion{
		ExpectedTools:       toolsRaw,
		ExpectedEgress:      egressRaw,
		RequiredCredentials: credsRaw,
		Reason:              reason,
	}, nil
}

// EnvelopeToRawColumns serializes the envelope arrays for storage as
// the per-field JSON columns on the tasks row. Empty arrays serialize
// to nil so the caller can pass to the store as "no change" / "default
// to []". Symmetric to EnvelopeFromTask: read and write go through one
// pair, so the four create/expand call sites can't drift on the
// per-field error handling.
func EnvelopeToRawColumns(env Envelope) (toolsRaw, egressRaw, credsRaw json.RawMessage, err error) {
	if len(env.ExpectedTools) > 0 {
		b, mErr := json.Marshal(env.ExpectedTools)
		if mErr != nil {
			return nil, nil, nil, mErr
		}
		toolsRaw = b
	}
	if len(env.ExpectedEgress) > 0 {
		b, mErr := json.Marshal(env.ExpectedEgress)
		if mErr != nil {
			return nil, nil, nil, mErr
		}
		egressRaw = b
	}
	if len(env.RequiredCredentials) > 0 {
		b, mErr := json.Marshal(env.RequiredCredentials)
		if mErr != nil {
			return nil, nil, nil, mErr
		}
		credsRaw = b
	}
	return toolsRaw, egressRaw, credsRaw, nil
}
