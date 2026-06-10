package handlers

import (
	"encoding/json"
	"reflect"
	"testing"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestBuildExpansionApprovalUpdate_ReplaceByName covers the
// load-bearing dedup contract: an expansion that names an existing
// tool wholesale replaces its `why` rather than appending a duplicate.
// The merged ExpectedTools that gets persisted has exactly one entry
// per canonical tool name.
func TestBuildExpansionApprovalUpdate_ReplaceByName(t *testing.T) {
	parentTools := mustMarshal(t, []runtimetasks.ExpectedTool{
		{ToolName: "Bash", Why: "Run a single curl to list emails"},
	})
	pending := mustMarshal(t, []runtimetasks.ExpectedTool{
		{ToolName: "Bash", Why: "List emails AND run processing script"},
		{ToolName: "Edit", Why: "Apply fixes to the processing script"},
	})
	task := &store.Task{
		ExpectedTools: parentTools,
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: pending,
			Reason:        "now need to process the listed emails locally",
		},
	}

	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}

	var merged []runtimetasks.ExpectedTool
	if err := json.Unmarshal(out.ExpectedTools, &merged); err != nil {
		t.Fatalf("unmarshal merged tools: %v", err)
	}
	want := []runtimetasks.ExpectedTool{
		{ToolName: "Bash", Why: "List emails AND run processing script"},
		{ToolName: "Edit", Why: "Apply fixes to the processing script"},
	}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("merged tools = %+v, want %+v", merged, want)
	}
}

// TestBuildExpansionApprovalUpdate_LocalToolKeepsActions confirms that
// expansion with a local-harness tool (no service:action shape) does
// NOT add a new AuthorizedAction. The envelope still tracks the entry,
// but local tools don't traverse the gateway path.
func TestBuildExpansionApprovalUpdate_LocalToolKeepsActions(t *testing.T) {
	parentActions := []store.TaskAction{
		{Service: "google.gmail", Action: "list_messages", AutoExecute: true},
	}
	task := &store.Task{
		AuthorizedActions: parentActions,
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "Edit", Why: "Apply fixes"},
			}),
			Reason: "need to write a script",
		},
	}
	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}
	if !reflect.DeepEqual(out.AuthorizedActions, parentActions) {
		t.Errorf("AuthorizedActions = %+v, want %+v (local tool must not derive a gateway scope)",
			out.AuthorizedActions, parentActions)
	}
}

// TestBuildExpansionApprovalUpdate_DerivesActionFromServiceColonAction
// is the load-bearing check: expansion with a tool named "service:action"
// MUST add a corresponding AuthorizedAction so the gateway's CheckTaskScope
// (which reads only AuthorizedActions) accepts the newly approved scope on
// subsequent /api/gateway/request calls.
func TestBuildExpansionApprovalUpdate_DerivesActionFromServiceColonAction(t *testing.T) {
	parentActions := []store.TaskAction{
		{Service: "mock.echo", Action: "echo", AutoExecute: true},
	}
	task := &store.Task{
		AuthorizedActions: parentActions,
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "mock.echo:other", Why: "Need to run other action for analysis"},
			}),
			Reason: "follow-up call needed",
		},
	}
	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}
	if got := len(out.AuthorizedActions); got != 2 {
		t.Fatalf("AuthorizedActions len = %d, want 2 (one existing + one derived)", got)
	}
	derived := out.AuthorizedActions[1]
	if derived.Service != "mock.echo" || derived.Action != "other" {
		t.Errorf("derived action = %s/%s, want mock.echo/other", derived.Service, derived.Action)
	}
	if derived.ExpansionRationale != "Need to run other action for analysis" {
		t.Errorf("ExpansionRationale = %q, want the per-entry why", derived.ExpansionRationale)
	}
	if derived.AutoExecute {
		t.Errorf("AutoExecute = true; derived actions must default to false (per-call approval) regardless of parent posture")
	}
}

// TestMergeAuthorizedActions_DerivedActionDefaultsToManual locks in
// the strict opt-in safety posture: a derived AuthorizedAction is
// ALWAYS created with AutoExecute=false, regardless of the parent's
// posture for other actions under the same service. This prevents a
// benign approval (e.g. github:list_issues with auto_execute=true)
// from silently extending unmediated execution to a destructive new
// action (e.g. github:delete_repo) on the same service. Users can
// relax per-action via the dashboard's scope overrides after approve.
func TestMergeAuthorizedActions_DerivedActionDefaultsToManual(t *testing.T) {
	parent := []store.TaskAction{
		// Parent has auto_execute=true on the same service — this
		// MUST NOT leak to the new action.
		{Service: "mock.echo", Action: "echo", AutoExecute: true},
	}
	additions := []runtimetasks.ExpectedTool{
		{ToolName: "mock.echo:other", Why: "Need other action on same service"},
		{ToolName: "github:create_issue", Why: "Net-new service"},
	}
	merged := mergeAuthorizedActionsFromExpansion(parent, additions)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3", len(merged))
	}
	for i := 1; i < len(merged); i++ {
		if merged[i].AutoExecute {
			t.Errorf("derived[%d] = %+v; AutoExecute must default to false on every derived action", i, merged[i])
		}
	}
	// Parent's existing entry is unaffected.
	if !merged[0].AutoExecute {
		t.Errorf("parent[0].AutoExecute changed: %+v", merged[0])
	}
}

// TestMergeAuthorizedActions_CaseInsensitiveDedup guards the cross-
// surface consistency contract: case-mismatched service/action names
// dedup against the parent (mirroring the ExpectedTools dedup), so a
// new "GitHub:Create_Issue" addition replaces an existing
// "github:create_issue" rather than landing as a duplicate
// AuthorizedAction while the envelope shows one entry.
func TestMergeAuthorizedActions_CaseInsensitiveDedup(t *testing.T) {
	parent := []store.TaskAction{
		{Service: "github", Action: "create_issue", AutoExecute: false, ExpansionRationale: "old"},
	}
	additions := []runtimetasks.ExpectedTool{
		{ToolName: "GitHub:Create_Issue", Why: "Broader reason"},
	}
	merged := mergeAuthorizedActionsFromExpansion(parent, additions)
	if len(merged) != 1 {
		t.Fatalf("merged len = %d, want 1 (case-insensitive dedup failed)", len(merged))
	}
	if merged[0].ExpansionRationale != "Broader reason" {
		t.Errorf("ExpansionRationale = %q, want the new why (replace-by-name failed)", merged[0].ExpansionRationale)
	}
}

// TestBuildExpansionApprovalUpdate_DedupReplacesActionRationale covers
// the (service, action) replace-by-name path: re-expanding an existing
// authorized action with a new per-entry why overwrites ExpansionRationale
// but does NOT duplicate the action.
func TestBuildExpansionApprovalUpdate_DedupReplacesActionRationale(t *testing.T) {
	parentActions := []store.TaskAction{
		{Service: "mock.echo", Action: "other", AutoExecute: true, ExpansionRationale: "original reason"},
	}
	task := &store.Task{
		AuthorizedActions: parentActions,
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "mock.echo:other", Why: "broader rationale that subsumes the prior"},
			}),
		},
	}
	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}
	if got := len(out.AuthorizedActions); got != 1 {
		t.Fatalf("AuthorizedActions len = %d, want 1 (dedup on service+action)", got)
	}
	if got := out.AuthorizedActions[0].ExpansionRationale; got != "broader rationale that subsumes the prior" {
		t.Errorf("ExpansionRationale = %q, want the new why (replace failed)", got)
	}
	// AutoExecute preserved from the parent on replace — don't silently
	// flip safety posture under the user.
	if !out.AuthorizedActions[0].AutoExecute {
		t.Errorf("AutoExecute = false; replace-by-name must preserve the prior value")
	}
}

// TestParseToolNameAsServiceAction covers the parser used by the
// derivation: bare names are local tools, single-colon names parse as
// service+action, multi-colon names treat everything before the last
// colon as the service id (so account-aliased ids work).
func TestParseToolNameAsServiceAction(t *testing.T) {
	cases := []struct {
		toolName        string
		wantService     string
		wantAction      string
		wantOK          bool
	}{
		{"Bash", "", "", false},
		{"", "", "", false},
		{":action", "", "", false},
		{"service:", "", "", false},
		{"mock.echo:echo", "mock.echo", "echo", true},
		{"google.gmail:work:list_messages", "google.gmail:work", "list_messages", true},
		{"  github:list_issues  ", "github", "list_issues", true},
	}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			s, a, ok := parseToolNameAsServiceAction(tc.toolName)
			if ok != tc.wantOK || s != tc.wantService || a != tc.wantAction {
				t.Errorf("parseToolNameAsServiceAction(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.toolName, s, a, ok, tc.wantService, tc.wantAction, tc.wantOK)
			}
		})
	}
}

// TestBuildExpansionApprovalUpdate_AppendsCredentials covers the
// add-new path in mergeRequiredCredentials. A net-new credential must
// be added to the merged envelope so subsequent placeholder minting
// has a real vault item to bind. (PR 1 only persists the envelope; PR
// 3 will wire the placeholder mint for new credentials.)
func TestBuildExpansionApprovalUpdate_AppendsCredentials(t *testing.T) {
	parentCreds := mustMarshal(t, []runtimetasks.RequiredCredential{
		{VaultItemID: "github:personal", Why: "List issues"},
	})
	pending := mustMarshal(t, []runtimetasks.RequiredCredential{
		{VaultItemID: "openai", Why: "Summarize the issue text"},
	})
	task := &store.Task{
		RequiredCredentials: parentCreds,
		PendingExpansion: &store.PendingTaskExpansion{
			RequiredCredentials: pending,
		},
	}
	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}
	var merged []runtimetasks.RequiredCredential
	if err := json.Unmarshal(out.RequiredCredentials, &merged); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := len(merged); got != 2 {
		t.Fatalf("merged credentials count = %d, want 2", got)
	}
	if merged[0].VaultItemID != "github:personal" || merged[1].VaultItemID != "openai" {
		t.Errorf("merged order / ids = %+v, want [github:personal, openai]", merged)
	}
}

// TestBuildExpansionApprovalUpdate_HandlesEmptyParent covers the case
// where the parent task was created via the v1 path with no envelope
// (only AuthorizedActions). The merge starts from an empty envelope
// and accepts every addition.
func TestBuildExpansionApprovalUpdate_HandlesEmptyParent(t *testing.T) {
	task := &store.Task{
		AuthorizedActions: []store.TaskAction{
			{Service: "test", Action: "*"},
		},
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "Bash", Why: "first envelope-shape tool"},
			}),
		},
	}
	out, err := buildExpansionApprovalUpdate(task)
	if err != nil {
		t.Fatalf("buildExpansionApprovalUpdate: %v", err)
	}
	var tools []runtimetasks.ExpectedTool
	if err := json.Unmarshal(out.ExpectedTools, &tools); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tools) != 1 || tools[0].ToolName != "Bash" {
		t.Errorf("merged tools = %+v, want one Bash entry", tools)
	}
}

// TestPendingExpansionToEnvelope_FailsOnCorruptJSON locks in fail-closed
// behavior: a malformed pending row at approve time bubbles a hard
// error rather than silently dropping the corrupt field. Silent decode
// would let the user approve a non-empty diff (rendered from the raw
// JSON before the corruption surfaced) only to have the store commit
// an empty merge — exactly the trust gap a corrupted pending row
// shouldn't be allowed to create.
func TestPendingExpansionToEnvelope_FailsOnCorruptJSON(t *testing.T) {
	_, err := pendingExpansionToEnvelope(&store.PendingTaskExpansion{
		ExpectedTools:       json.RawMessage(`not json`),
		ExpectedEgress:      mustMarshal(t, []runtimetasks.ExpectedEgress{{Host: "example.com", Why: "ok"}}),
		RequiredCredentials: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatalf("pendingExpansionToEnvelope: want error on corrupt expected_tools JSON, got nil")
	}
}

// TestDerivedActionsFromPending_ExcludesPriorExpansionRationale guards
// the filter regression: a task expanded once before keeps the
// ExpansionRationale on its previously-granted AuthorizedActions, and
// a subsequent expansion's PendingDerivedActions must NOT surface those
// prior actions as part of the current pending diff. Only the actions
// the CURRENT additions actually propose count.
func TestDerivedActionsFromPending_ExcludesPriorExpansionRationale(t *testing.T) {
	task := &store.Task{
		Status: "pending_scope_expansion",
		AuthorizedActions: []store.TaskAction{
			{Service: "mock.echo", Action: "echo", AutoExecute: true},
			// Granted by a PRIOR expansion — still carries the rationale.
			{Service: "mock.echo", Action: "other", AutoExecute: true, ExpansionRationale: "older expansion reason"},
		},
		PendingExpansion: &store.PendingTaskExpansion{
			// Current expansion proposes a DIFFERENT action; the prior
			// expansion's `other` must not appear in the derived diff.
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "mock.echo:third", Why: "Need third action for new step"},
			}),
		},
	}
	derived, err := derivedActionsFromPending(task)
	if err != nil {
		t.Fatalf("derivedActionsFromPending: %v", err)
	}
	if len(derived) != 1 {
		t.Fatalf("derived len = %d, want 1 (prior expansion's `other` must not leak)", len(derived))
	}
	if derived[0].Action != "third" {
		t.Errorf("derived[0].Action = %q, want %q", derived[0].Action, "third")
	}
}

// TestDerivedActionsFromPending_IncludesReplacementOfPriorAction covers
// the legitimate re-expansion path: when the CURRENT additions name an
// action that was already on the task, it appears in the diff with the
// freshly-supplied rationale (replace-by-name on the gateway scope).
func TestDerivedActionsFromPending_IncludesReplacementOfPriorAction(t *testing.T) {
	task := &store.Task{
		Status: "pending_scope_expansion",
		AuthorizedActions: []store.TaskAction{
			{Service: "mock.echo", Action: "other", AutoExecute: true, ExpansionRationale: "older reason"},
		},
		PendingExpansion: &store.PendingTaskExpansion{
			ExpectedTools: mustMarshal(t, []runtimetasks.ExpectedTool{
				{ToolName: "mock.echo:other", Why: "Broader reason that subsumes the prior"},
			}),
		},
	}
	derived, err := derivedActionsFromPending(task)
	if err != nil {
		t.Fatalf("derivedActionsFromPending: %v", err)
	}
	if len(derived) != 1 {
		t.Fatalf("derived len = %d, want 1", len(derived))
	}
	if got, want := derived[0].ExpansionRationale, "Broader reason that subsumes the prior"; got != want {
		t.Errorf("rationale = %q, want %q (replace-by-name didn't fire)", got, want)
	}
}

// TestPendingExpansionToEnvelope_AcceptsWellFormed is the success-path
// guard: when every field decodes cleanly, the envelope is returned
// without error.
func TestPendingExpansionToEnvelope_AcceptsWellFormed(t *testing.T) {
	env, err := pendingExpansionToEnvelope(&store.PendingTaskExpansion{
		ExpectedTools:  mustMarshal(t, []runtimetasks.ExpectedTool{{ToolName: "Bash", Why: "ok"}}),
		ExpectedEgress: mustMarshal(t, []runtimetasks.ExpectedEgress{{Host: "example.com", Why: "ok"}}),
	})
	if err != nil {
		t.Fatalf("pendingExpansionToEnvelope: %v", err)
	}
	if len(env.ExpectedTools) != 1 || len(env.ExpectedEgress) != 1 {
		t.Errorf("env = %+v, want one tool and one egress entry", env)
	}
}

// TestEnvelopeHasAdditions covers the empty-body rejection — Expand
// must refuse a no-op body rather than silently flipping the task to
// pending_scope_expansion.
func TestEnvelopeHasAdditions(t *testing.T) {
	cases := []struct {
		name string
		env  runtimetasks.Envelope
		want bool
	}{
		{"empty", runtimetasks.Envelope{}, false},
		{"tools only", runtimetasks.Envelope{
			ExpectedTools: []runtimetasks.ExpectedTool{{ToolName: "Bash"}},
		}, true},
		{"egress only", runtimetasks.Envelope{
			ExpectedEgress: []runtimetasks.ExpectedEgress{{Host: "example.com"}},
		}, true},
		{"credentials only", runtimetasks.Envelope{
			RequiredCredentials: []runtimetasks.RequiredCredential{{VaultItemID: "x"}},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envelopeHasAdditions(tc.env); got != tc.want {
				t.Errorf("envelopeHasAdditions(%+v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
