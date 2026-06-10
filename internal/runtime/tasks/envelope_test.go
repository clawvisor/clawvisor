package tasks

import (
	"reflect"
	"testing"
)

func TestMergeEnvelopes_AppendNewTools(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "Run a single curl to list emails"},
		},
	}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Edit", Why: "Apply fixes to processing script"},
		},
	}
	res := MergeEnvelopes(parent, additions)

	if got, want := len(res.Merged.ExpectedTools), 2; got != want {
		t.Fatalf("merged tools = %d, want %d", got, want)
	}
	if got, want := res.Merged.ExpectedTools[0].ToolName, "Bash"; got != want {
		t.Errorf("tools[0] = %q, want %q", got, want)
	}
	if got, want := res.Merged.ExpectedTools[1].ToolName, "Edit"; got != want {
		t.Errorf("tools[1] = %q, want %q", got, want)
	}
	if len(res.AddedTools) != 1 || res.AddedTools[0].ToolName != "Edit" {
		t.Errorf("AddedTools = %v, want [Edit]", res.AddedTools)
	}
	if len(res.ReplacedTools) != 0 {
		t.Errorf("ReplacedTools = %v, want empty", res.ReplacedTools)
	}
}

func TestMergeEnvelopes_ReplacesWhyOnDuplicateTool(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "Run a single curl to list emails"},
		},
	}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "List emails AND run processing script"},
		},
	}
	res := MergeEnvelopes(parent, additions)

	if got, want := len(res.Merged.ExpectedTools), 1; got != want {
		t.Fatalf("merged tools = %d, want %d (dedup-by-name failed)", got, want)
	}
	if got, want := res.Merged.ExpectedTools[0].Why, "List emails AND run processing script"; got != want {
		t.Errorf("Why = %q, want %q (replace failed)", got, want)
	}
	if len(res.AddedTools) != 0 {
		t.Errorf("AddedTools = %v, want empty", res.AddedTools)
	}
	if got, want := len(res.ReplacedTools), 1; got != want {
		t.Fatalf("ReplacedTools len = %d, want %d", got, want)
	}
	if got, want := res.ReplacedTools[0].Prior.Why, "Run a single curl to list emails"; got != want {
		t.Errorf("ReplacedTools[0].Prior.Why = %q, want the prior why %q", got, want)
	}
	if got, want := res.ReplacedTools[0].New.Why, "List emails AND run processing script"; got != want {
		t.Errorf("ReplacedTools[0].New.Why = %q, want the new why %q", got, want)
	}
}

func TestMergeEnvelopes_ToolNameDedupIsCaseInsensitive(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{{ToolName: "Bash", Why: "old"}},
	}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{{ToolName: "bash", Why: "new"}},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedTools); got != 1 {
		t.Fatalf("merged tools = %d, want 1", got)
	}
	if got, want := res.Merged.ExpectedTools[0].Why, "new"; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
	// Preserve the PARENT's casing on replacement — replace-by-name is a
	// why update, not a rename. Otherwise the approval prompt renders
	// `bash` replacing `Bash`, which looks like a new entry.
	if got, want := res.Merged.ExpectedTools[0].ToolName, "Bash"; got != want {
		t.Errorf("ToolName = %q, want %q (parent casing preserved)", got, want)
	}
}

// TestMergeEnvelopes_ToolReplacePreservesInputShape locks in the
// non-why field preservation contract. An addition that names the
// same tool but with a different (or absent) InputShape /
// InputRegex must NOT silently widen the parent's previously-approved
// shape — the approval prompts only diff `why`, so a structural
// relaxation would land invisibly.
func TestMergeEnvelopes_ToolReplacePreservesInputShape(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{
			{
				ToolName:   "Bash",
				Why:        "Run a single curl to list emails",
				InputShape: map[string]any{"cmd": map[string]any{"contains": "curl"}},
				InputRegex: `^curl `,
			},
		},
	}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{
			// Note: the agent omits InputShape/InputRegex — without
			// preservation, this would silently relax the parent's
			// shape constraints.
			{ToolName: "Bash", Why: "List emails AND run a local processing script"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedTools); got != 1 {
		t.Fatalf("merged tools = %d, want 1", got)
	}
	merged := res.Merged.ExpectedTools[0]
	if merged.Why != "List emails AND run a local processing script" {
		t.Errorf("Why = %q, want the new why", merged.Why)
	}
	if merged.InputRegex != `^curl ` {
		t.Errorf("InputRegex = %q; parent's regex was silently dropped", merged.InputRegex)
	}
	if merged.InputShape == nil {
		t.Errorf("InputShape = nil; parent's shape was silently dropped")
	}
}

// TestMergeEnvelopes_EgressStructuralMismatchAppends is the
// load-bearing P1 fix: when an expansion names the same host with
// DIFFERENT structural fields (Method, Path, …), the addition lands
// as a NEW row rather than silently collapsing onto the parent. The
// previous behavior — "same host, replace only why, preserve parent's
// shape" — showed the addition's POST /orgs in the approval prompt
// but persisted the parent's GET /user/repos, so the user approved
// a delta that never actually authorized any new traffic. The
// reviewer must approve what they see.
func TestMergeEnvelopes_EgressStructuralMismatchAppends(t *testing.T) {
	parent := Envelope{
		ExpectedEgress: []ExpectedEgress{
			{
				Host:       "api.github.com",
				Why:        "List repos for the user",
				Method:     "GET",
				Path:       "/user/repos",
				QueryShape: map[string]any{"per_page": map[string]any{"<=": 100}},
			},
		},
	}
	additions := Envelope{
		ExpectedEgress: []ExpectedEgress{
			{Host: "api.github.com", Why: "Comment on issues", Method: "POST", Path: "/repos/{owner}/{repo}/issues/{n}/comments"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedEgress); got != 2 {
		t.Fatalf("merged egress = %d, want 2 (structural delta must NOT collapse onto the parent)", got)
	}
	// Parent row unchanged.
	if res.Merged.ExpectedEgress[0].Method != "GET" || res.Merged.ExpectedEgress[0].Path != "/user/repos" {
		t.Errorf("parent row was modified: %+v", res.Merged.ExpectedEgress[0])
	}
	if res.Merged.ExpectedEgress[0].Why != "List repos for the user" {
		t.Errorf("parent Why = %q, want unchanged", res.Merged.ExpectedEgress[0].Why)
	}
	// Addition lands intact as a new row.
	added := res.Merged.ExpectedEgress[1]
	if added.Method != "POST" || added.Path != "/repos/{owner}/{repo}/issues/{n}/comments" {
		t.Errorf("addition row = %+v, want POST/.../comments", added)
	}
	if added.Why != "Comment on issues" {
		t.Errorf("addition Why = %q, want the new why", added.Why)
	}
	if len(res.AddedEgress) != 1 || res.AddedEgress[0].Path != "/repos/{owner}/{repo}/issues/{n}/comments" {
		t.Errorf("AddedEgress = %+v, want one entry for the new endpoint", res.AddedEgress)
	}
	if len(res.ReplacedEgress) != 0 {
		t.Errorf("ReplacedEgress = %+v, want empty (structural mismatch is NOT a replacement)", res.ReplacedEgress)
	}
}

// TestMergeEnvelopes_EgressWhyOnlyUpdatePreservesShape locks in the
// other side of the structural rule: an addition that restates a host
// WITHOUT structural fields is a why-update and preserves the parent's
// shape. Silently relaxing the parent's Method/Path/regex would widen
// the previously approved scope without a signal in the approval
// prompt (which only diffs `why`).
func TestMergeEnvelopes_EgressWhyOnlyUpdatePreservesShape(t *testing.T) {
	parent := Envelope{
		ExpectedEgress: []ExpectedEgress{
			{
				Host:       "api.github.com",
				Why:        "List repos for the user",
				Method:     "GET",
				Path:       "/user/repos",
				QueryShape: map[string]any{"per_page": map[string]any{"<=": 100}},
			},
		},
	}
	additions := Envelope{
		ExpectedEgress: []ExpectedEgress{
			// No method/path → agent is only revising the rationale.
			{Host: "api.github.com", Why: "List repos AND pre-cache for the daily digest"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedEgress); got != 1 {
		t.Fatalf("merged egress = %d, want 1", got)
	}
	merged := res.Merged.ExpectedEgress[0]
	if merged.Method != "GET" {
		t.Errorf("Method = %q; parent's GET was silently dropped by an empty-method why-update", merged.Method)
	}
	if merged.Path != "/user/repos" {
		t.Errorf("Path = %q; parent's Path was silently dropped", merged.Path)
	}
	if merged.QueryShape == nil {
		t.Errorf("QueryShape = nil; parent's structural constraint was silently dropped")
	}
	if merged.Why != "List repos AND pre-cache for the daily digest" {
		t.Errorf("Why = %q, want the new why", merged.Why)
	}
	if len(res.ReplacedEgress) != 1 {
		t.Errorf("ReplacedEgress len = %d, want 1", len(res.ReplacedEgress))
	}
}

// TestMergeEnvelopes_ToolStructuralMismatchAppends is the tool-side
// equivalent of EgressStructuralMismatchAppends. An addition restates
// the same tool name with a different InputRegex → distinct endpoint
// under the same harness tool, lands as a new row.
func TestMergeEnvelopes_ToolStructuralMismatchAppends(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "list emails", InputRegex: `^curl `},
		},
	}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "run the processing script", InputRegex: `^python `},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedTools); got != 2 {
		t.Fatalf("merged tools = %d, want 2 (different InputRegex = different endpoint)", got)
	}
	if res.Merged.ExpectedTools[0].InputRegex != `^curl ` {
		t.Errorf("parent InputRegex was modified: %q", res.Merged.ExpectedTools[0].InputRegex)
	}
	if res.Merged.ExpectedTools[1].InputRegex != `^python ` {
		t.Errorf("addition InputRegex = %q, want ^python ", res.Merged.ExpectedTools[1].InputRegex)
	}
	if len(res.AddedTools) != 1 {
		t.Errorf("AddedTools = %+v, want one new entry", res.AddedTools)
	}
	if len(res.ReplacedTools) != 0 {
		t.Errorf("ReplacedTools = %+v, want empty", res.ReplacedTools)
	}
}

// TestMergeEnvelopes_CredentialReplacePreservesParentCasing locks in
// the case-insensitive dedup + parent-casing-preserved contract for
// credentials. The vault lookup is case-sensitive on the stored
// identifier, so re-stamping the parent's `github:foo` to the
// addition's `GITHUB:FOO` would silently change the lookup target.
// Mirrors the tools/egress preservation rule.
func TestMergeEnvelopes_CredentialReplacePreservesParentCasing(t *testing.T) {
	parent := Envelope{
		RequiredCredentials: []RequiredCredential{
			{VaultItemID: "github:foo", Why: "List issues"},
		},
	}
	additions := Envelope{
		RequiredCredentials: []RequiredCredential{
			{VaultItemID: "GITHUB:FOO", Why: "Comment on issues"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.RequiredCredentials); got != 1 {
		t.Fatalf("merged credentials = %d, want 1 (case-insensitive dedup)", got)
	}
	if got, want := res.Merged.RequiredCredentials[0].VaultItemID, "github:foo"; got != want {
		t.Errorf("VaultItemID = %q, want %q (parent casing must be preserved on replace)", got, want)
	}
	if got, want := res.Merged.RequiredCredentials[0].Why, "Comment on issues"; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
	if len(res.ReplacedCredentials) != 1 {
		t.Errorf("ReplacedCredentials = %v, want one entry", res.ReplacedCredentials)
	}
}

func TestMergeEnvelopes_EgressByHost(t *testing.T) {
	parent := Envelope{
		ExpectedEgress: []ExpectedEgress{
			{Host: "api.github.com", Why: "List issues"},
		},
	}
	additions := Envelope{
		ExpectedEgress: []ExpectedEgress{
			{Host: "API.GitHub.com", Why: "List AND comment on issues"},
			{Host: "api.openai.com", Why: "Summarize the issue text"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedEgress); got != 2 {
		t.Fatalf("merged egress = %d, want 2", got)
	}
	if got, want := res.Merged.ExpectedEgress[0].Why, "List AND comment on issues"; got != want {
		t.Errorf("egress[0].Why = %q, want %q", got, want)
	}
	if got, want := res.Merged.ExpectedEgress[1].Host, "api.openai.com"; got != want {
		t.Errorf("egress[1].Host = %q, want %q", got, want)
	}
	if len(res.ReplacedEgress) != 1 || res.ReplacedEgress[0].Prior.Why != "List issues" {
		t.Errorf("ReplacedEgress = %+v, want one entry with the old why", res.ReplacedEgress)
	}
	if res.ReplacedEgress[0].New.Why != "List AND comment on issues" {
		t.Errorf("ReplacedEgress[0].New.Why = %q, want the new why", res.ReplacedEgress[0].New.Why)
	}
	if len(res.AddedEgress) != 1 || res.AddedEgress[0].Host != "api.openai.com" {
		t.Errorf("AddedEgress = %+v, want [api.openai.com]", res.AddedEgress)
	}
}

// TestMergeEnvelopes_CredentialKeyIsKindScoped asserts that
// vault_item_id and vault_item_handle do NOT collide even when their
// values match. They name credentials through different lookup paths,
// and the wholesale-replace dedup would otherwise leave the merged
// entry with only one of the two identifier fields populated —
// silently swapping the resolution path the parent task was set up for.
func TestMergeEnvelopes_CredentialKeyIsKindScoped(t *testing.T) {
	parent := Envelope{
		RequiredCredentials: []RequiredCredential{
			{VaultItemHandle: "github:personal", Why: "List issues"},
		},
	}
	additions := Envelope{
		RequiredCredentials: []RequiredCredential{
			// Same string value, but expressed through a different
			// identifier kind. The two must NOT collide — the runtime
			// resolves each via a different path.
			{VaultItemID: "github:personal", Why: "Create issues"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.RequiredCredentials); got != 2 {
		t.Fatalf("merged credentials = %d, want 2 (id vs handle must not collide)", got)
	}
	if len(res.AddedCredentials) != 1 || res.AddedCredentials[0].VaultItemID != "github:personal" {
		t.Errorf("AddedCredentials = %+v, want one id-keyed entry", res.AddedCredentials)
	}
	if len(res.ReplacedCredentials) != 0 {
		t.Errorf("ReplacedCredentials = %v, want empty (kind mismatch is not a replacement)", res.ReplacedCredentials)
	}
}

// TestMergeEnvelopes_CredentialReplaceWithinSameKind covers the
// inside-kind dedup contract: two entries that both use vault_item_id
// for the same value DO collide as a replace-by-name.
func TestMergeEnvelopes_CredentialReplaceWithinSameKind(t *testing.T) {
	parent := Envelope{
		RequiredCredentials: []RequiredCredential{
			{VaultItemID: "github:personal", Why: "List issues"},
		},
	}
	additions := Envelope{
		RequiredCredentials: []RequiredCredential{
			{VaultItemID: "github:personal", Why: "List AND comment on issues"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.RequiredCredentials); got != 1 {
		t.Fatalf("merged credentials = %d, want 1 (same kind, same value should dedup)", got)
	}
	if got, want := res.Merged.RequiredCredentials[0].Why, "List AND comment on issues"; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
	if len(res.ReplacedCredentials) != 1 {
		t.Errorf("ReplacedCredentials = %v, want one entry", res.ReplacedCredentials)
	}
}

func TestMergeEnvelopes_EmptyAdditionsKeepsParent(t *testing.T) {
	parent := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "Bash", Why: "Existing"},
		},
		IntentVerificationMode: "strict",
		ExpectedUse:            "kept",
		SchemaVersion:          2,
	}
	res := MergeEnvelopes(parent, Envelope{})

	if !reflect.DeepEqual(res.Merged.ExpectedTools, parent.ExpectedTools) {
		t.Errorf("ExpectedTools changed: got %+v, want %+v", res.Merged.ExpectedTools, parent.ExpectedTools)
	}
	if res.Merged.IntentVerificationMode != "strict" {
		t.Errorf("IntentVerificationMode = %q, want strict", res.Merged.IntentVerificationMode)
	}
	if res.Merged.ExpectedUse != "kept" {
		t.Errorf("ExpectedUse = %q, want kept", res.Merged.ExpectedUse)
	}
	if res.Merged.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", res.Merged.SchemaVersion)
	}
	if len(res.AddedTools)+len(res.ReplacedTools) != 0 {
		t.Errorf("expected no diff lists for empty additions, got added=%v replaced=%v", res.AddedTools, res.ReplacedTools)
	}
}

func TestMergeEnvelopes_EmptyParentTakesAdditions(t *testing.T) {
	additions := Envelope{
		ExpectedTools: []ExpectedTool{{ToolName: "Bash", Why: "Fresh"}},
		SchemaVersion: 2,
	}
	res := MergeEnvelopes(Envelope{}, additions)

	if got := len(res.Merged.ExpectedTools); got != 1 {
		t.Fatalf("merged tools = %d, want 1", got)
	}
	if got, want := res.Merged.ExpectedTools[0].Why, "Fresh"; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
	if len(res.AddedTools) != 1 {
		t.Errorf("AddedTools should record the fresh entry, got %v", res.AddedTools)
	}
	if res.Merged.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", res.Merged.SchemaVersion)
	}
}

func TestMergeEnvelopes_EmptyKeysSkipped(t *testing.T) {
	parent := Envelope{}
	additions := Envelope{
		ExpectedTools: []ExpectedTool{
			{ToolName: "", Why: "no name should be dropped"},
			{ToolName: "Bash", Why: "kept"},
		},
		ExpectedEgress: []ExpectedEgress{
			{Host: "", Why: "no host"},
		},
		RequiredCredentials: []RequiredCredential{
			{Why: "no vault item id or handle"},
		},
	}
	res := MergeEnvelopes(parent, additions)
	if got := len(res.Merged.ExpectedTools); got != 1 {
		t.Errorf("merged tools = %d, want 1 (empty name dropped)", got)
	}
	if got := len(res.Merged.ExpectedEgress); got != 0 {
		t.Errorf("merged egress = %d, want 0 (empty host dropped)", got)
	}
	if got := len(res.Merged.RequiredCredentials); got != 0 {
		t.Errorf("merged credentials = %d, want 0 (empty id/handle dropped)", got)
	}
}
