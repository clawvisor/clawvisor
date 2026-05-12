package llmproxy

import (
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

func githubDef() yamldef.ServiceDef {
	return yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "github"},
		API:     yamldef.APIDef{BaseURL: "https://api.github.com", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"list_issues": {Method: "GET", Path: "/repos/{{.owner}}/{{.repo}}/issues"},
			"get_issue":   {Method: "GET", Path: "/repos/{{.owner}}/{{.repo}}/issues/{{.number}}"},
			"create_issue": {Method: "POST", Path: "/repos/{{.owner}}/{{.repo}}/issues"},
			"get_user":    {Method: "GET", Path: "/user"},
		},
	}
}

func openaiDef() yamldef.ServiceDef {
	return yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "openai"},
		API:     yamldef.APIDef{BaseURL: "https://api.openai.com", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"chat_completions": {Method: "POST", Path: "/v1/chat/completions"},
		},
	}
}

func TestServiceCatalog_StaticPath(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})
	got, ok := c.Resolve("api.github.com", "GET", "/user")
	if !ok || got.ServiceID != "github" || got.ActionID != "get_user" {
		t.Fatalf("got %+v ok=%v, want github/get_user", got, ok)
	}
}

func TestServiceCatalog_TemplatedPath(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})
	got, ok := c.Resolve("api.github.com", "GET", "/repos/clawvisor/clawvisor/issues")
	if !ok {
		t.Fatalf("expected resolve, got !ok")
	}
	if got.ActionID != "list_issues" {
		t.Errorf("action=%q, want list_issues", got.ActionID)
	}
}

func TestServiceCatalog_PrefersMostSpecificMatch(t *testing.T) {
	// Both actions could match /repos/x/y/issues/42 in theory if we got
	// regex anchoring wrong. The template with more static segments
	// should win for /repos/x/y/issues/42 (get_issue), and the shorter
	// one should win for /repos/x/y/issues (list_issues).
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})

	got, ok := c.Resolve("api.github.com", "GET", "/repos/x/y/issues/42")
	if !ok || got.ActionID != "get_issue" {
		t.Errorf("expected get_issue, got %+v ok=%v", got, ok)
	}
	got, ok = c.Resolve("api.github.com", "GET", "/repos/x/y/issues")
	if !ok || got.ActionID != "list_issues" {
		t.Errorf("expected list_issues, got %+v ok=%v", got, ok)
	}
}

func TestServiceCatalog_MethodMatters(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})
	if _, ok := c.Resolve("api.github.com", "DELETE", "/repos/x/y/issues"); ok {
		t.Errorf("DELETE should not match list_issues GET")
	}
	got, _ := c.Resolve("api.github.com", "POST", "/repos/x/y/issues")
	if got.ActionID != "create_issue" {
		t.Errorf("POST should match create_issue, got %s", got.ActionID)
	}
}

func TestServiceCatalog_UnknownHost(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})
	if _, ok := c.Resolve("evil.example.com", "GET", "/repos/x/y/issues"); ok {
		t.Errorf("unknown host should not resolve")
	}
}

func TestServiceCatalog_PathNormalization(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef()})
	cases := []string{
		"/user?foo=bar",
		"/user#frag",
		"/user/",
		"user", // no leading slash
	}
	for _, p := range cases {
		got, ok := c.Resolve("api.github.com", "GET", p)
		if !ok || got.ActionID != "get_user" {
			t.Errorf("path %q: got %+v ok=%v", p, got, ok)
		}
	}
}

func TestServiceCatalog_MultipleServices(t *testing.T) {
	c := NewServiceCatalog([]yamldef.ServiceDef{githubDef(), openaiDef()})
	got, ok := c.Resolve("api.openai.com", "POST", "/v1/chat/completions")
	if !ok || got.ServiceID != "openai" {
		t.Errorf("expected openai, got %+v ok=%v", got, ok)
	}
	got, ok = c.Resolve("api.github.com", "GET", "/user")
	if !ok || got.ServiceID != "github" {
		t.Errorf("expected github, got %+v ok=%v", got, ok)
	}
}

func TestServiceCatalog_SkipsNonRest(t *testing.T) {
	gql := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "linear"},
		API:     yamldef.APIDef{BaseURL: "https://api.linear.app", Type: "graphql"},
		Actions: map[string]yamldef.Action{"q": {Query: "..."}},
	}
	c := NewServiceCatalog([]yamldef.ServiceDef{gql})
	if _, ok := c.Resolve("api.linear.app", "POST", "/graphql"); ok {
		t.Errorf("graphql service should not be resolvable by host/path")
	}
}

func TestServiceCatalog_SkipsTemplatedHost(t *testing.T) {
	tenanted := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "tenant"},
		API:     yamldef.APIDef{BaseURL: "https://{{.workspace}}.example.com", Type: "rest"},
		Actions: map[string]yamldef.Action{"x": {Method: "GET", Path: "/x"}},
	}
	c := NewServiceCatalog([]yamldef.ServiceDef{tenanted})
	if _, ok := c.Resolve("foo.example.com", "GET", "/x"); ok {
		t.Errorf("templated-host service should be skipped")
	}
}

// When two actions have identical specificity for the same request,
// the resolved (service, action) must be deterministic across catalog
// builds. Pre-fix, ties resolved by entry insertion order — stable
// within a process but invisible to operators making config changes.
func TestServiceCatalog_TieBreakIsDeterministic(t *testing.T) {
	// Two services exposing the same shape: GET / on the same host.
	makeDef := func(id, action string) yamldef.ServiceDef {
		return yamldef.ServiceDef{
			Service: yamldef.ServiceInfo{ID: id},
			API:     yamldef.APIDef{BaseURL: "https://overlap.example", Type: "rest"},
			Actions: map[string]yamldef.Action{
				action: {Method: "GET", Path: "/"},
			},
		}
	}
	// Build twice with the defs in different orders.
	a := NewServiceCatalog([]yamldef.ServiceDef{makeDef("alpha", "act_a"), makeDef("beta", "act_b")})
	b := NewServiceCatalog([]yamldef.ServiceDef{makeDef("beta", "act_b"), makeDef("alpha", "act_a")})
	resA, okA := a.Resolve("overlap.example", "GET", "/")
	resB, okB := b.Resolve("overlap.example", "GET", "/")
	if !okA || !okB {
		t.Fatalf("expected resolve, got okA=%v okB=%v", okA, okB)
	}
	if resA.ServiceID != resB.ServiceID || resA.ActionID != resB.ActionID {
		t.Fatalf("tie-break not deterministic: a=%+v b=%+v", resA, resB)
	}
	// Lexical: "alpha" < "beta" so alpha wins regardless of insertion.
	if resA.ServiceID != "alpha" {
		t.Errorf("expected lex-first winner (alpha), got %q", resA.ServiceID)
	}
}

func TestLazyServiceCatalog_RebuildsOnSet(t *testing.T) {
	l := NewLazyServiceCatalog([]yamldef.ServiceDef{githubDef()})
	if _, ok := l.Resolve("api.openai.com", "POST", "/v1/chat/completions"); ok {
		t.Fatalf("openai not yet in catalog, should not resolve")
	}
	l.SetDefinitions([]yamldef.ServiceDef{githubDef(), openaiDef()})
	got, ok := l.Resolve("api.openai.com", "POST", "/v1/chat/completions")
	if !ok || got.ServiceID != "openai" {
		t.Errorf("expected openai after Set, got %+v ok=%v", got, ok)
	}
}

// Regression: services whose base_url includes a path prefix
// (e.g. `https://api.example.com/v1`) must resolve when an
// inbound request includes that prefix.
func TestServiceCatalog_HonorsBaseURLPath(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "stripe"},
		API:     yamldef.APIDef{BaseURL: "https://api.stripe.com/v1", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"charges_list": {Method: "GET", Path: "/charges"},
		},
	}
	c := NewServiceCatalog([]yamldef.ServiceDef{def})
	got, ok := c.Resolve("api.stripe.com", "GET", "/v1/charges")
	if !ok {
		t.Fatalf("expected resolve for /v1/charges, got !ok")
	}
	if got.ActionID != "charges_list" {
		t.Errorf("action=%q, want charges_list", got.ActionID)
	}
}

// Regression: a templated path on a concrete host must still be
// indexed. Previously, hostFromBaseURL bailed on any `{{` anywhere
// in the URL, dropping Twilio-style base URLs entirely.
func TestServiceCatalog_AllowsTemplatesInPath(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "twilio"},
		API:     yamldef.APIDef{BaseURL: "https://api.twilio.com/2010-04-01/Accounts/{{.var.account_sid}}", Type: "rest"},
		Actions: map[string]yamldef.Action{
			"send_message": {Method: "POST", Path: "/Messages.json"},
		},
	}
	c := NewServiceCatalog([]yamldef.ServiceDef{def})
	got, ok := c.Resolve("api.twilio.com", "POST", "/2010-04-01/Accounts/AC123/Messages.json")
	if !ok {
		t.Fatalf("expected resolve for templated-path base URL")
	}
	if got.ServiceID != "twilio" {
		t.Errorf("serviceID=%q, want twilio", got.ServiceID)
	}
}
