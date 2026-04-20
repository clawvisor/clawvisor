package policy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

const samplePolicy = `
version: 1
name: example
rules:
  fast:
    - name: allow_reads
      action: allow
      match:
        methods: [GET]
        hosts: ["api.github.com"]
    - name: block_repo_delete
      action: block
      match:
        methods: [DELETE]
        hosts: ["api.github.com"]
        paths: ["/repos/*/*"]
      message: "repo deletion blocked"
    - name: flag_candidate_reject
      action: flag
      match:
        hosts: ["api.greenhouse.io"]
        paths: ["/v1/candidates/*/reject"]
  judge:
    enabled: true
    model: claude-haiku-4-5
    timeout_ms: 5000
    on_error: block
  default: allow
ban:
  enabled: true
  max_violations: 3
  window: 1h
  ban_duration: 30m
  scope: per_rule
`

func compileSample(t *testing.T) *CompiledPolicy {
	t.Helper()
	p, err := Parse([]byte(samplePolicy))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := Compile(p)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func mkReq(t *testing.T, method, urlStr string) *http.Request {
	t.Helper()
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: method, URL: u, Header: http.Header{}}
}

func TestCompile_Basic(t *testing.T) {
	c := compileSample(t)
	if c.Version != 1 {
		t.Errorf("version: %d", c.Version)
	}
	if c.Name != "example" {
		t.Errorf("name: %s", c.Name)
	}
	if len(c.Rules) != 3 {
		t.Errorf("rules: %d", len(c.Rules))
	}
	if c.DefaultAction != ActionAllow {
		t.Errorf("default: %s", c.DefaultAction)
	}
	if !c.Ban.Enabled || c.Ban.Window != time.Hour || c.Ban.Duration != 30*time.Minute {
		t.Errorf("ban: %+v", c.Ban)
	}
}

func TestEvaluate_AllowsReadWithFirstRule(t *testing.T) {
	c := compileSample(t)
	req := mkReq(t, "GET", "https://api.github.com/repos/alice/x")
	d := Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionAllow {
		t.Fatalf("expected allow, got %s", d.Action)
	}
	if d.Rule == nil || d.Rule.Name != "allow_reads" {
		t.Errorf("wrong rule: %+v", d.Rule)
	}
}

func TestEvaluate_BlocksRepoDelete(t *testing.T) {
	c := compileSample(t)
	req := mkReq(t, "DELETE", "https://api.github.com/repos/alice/my-repo")
	d := Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionBlock {
		t.Fatalf("expected block, got %s (rule=%+v)", d.Action, d.Rule)
	}
	if d.Rule.Message == "" {
		t.Error("expected message on block rule")
	}
}

func TestEvaluate_FlagsCandidateReject(t *testing.T) {
	c := compileSample(t)
	req := mkReq(t, "POST", "https://api.greenhouse.io/v1/candidates/123/reject")
	d := Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionFlag {
		t.Fatalf("expected flag, got %s", d.Action)
	}
}

func TestEvaluate_DefaultAction(t *testing.T) {
	c := compileSample(t)
	// Unknown host → no rule matches → default action (allow).
	req := mkReq(t, "GET", "https://unknown.example.com/")
	d := Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionAllow {
		t.Fatalf("expected default allow, got %s", d.Action)
	}
	if d.Rule != nil {
		t.Errorf("expected nil rule for default, got %+v", d.Rule)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"api.github.com", "api.github.com", true},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com", false},
		{"**", "anything/at/all", true},
		{"/repos/*/*", "/repos/alice/my-repo", true},
		{"/repos/*/*", "/repos/alice/nested/repo", false}, // * doesn't cross /
		{"/repos/**", "/repos/alice/nested/repo", true},
		{"/v1/candidates/*/reject", "/v1/candidates/123/reject", true},
		{"/v1/candidates/*/reject", "/v1/candidates/x/y/reject", false},
		{"", "", true},
		{"", "a", false},
	}
	for _, c := range cases {
		got := globMatch(c.pattern, c.s)
		if got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestEvaluate_AgentScoping(t *testing.T) {
	src := `
version: 1
name: agent-scoped
rules:
  fast:
    - name: allow_only_prod_agent
      action: allow
      match:
        hosts: ["api.github.com"]
        agents: ["agent-prod"]
    - name: block_everyone_else
      action: block
      match:
        hosts: ["api.github.com"]
  default: allow
`
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	req := mkReq(t, "GET", "https://api.github.com/")

	// Prod agent: first rule matches → allow.
	d := Evaluate(c, NewMatchContext(req, "agent-prod"))
	if d.Action != ActionAllow {
		t.Errorf("prod agent: expected allow, got %s", d.Action)
	}
	// Other agent: first rule requires prod agent → skipped → second rule blocks.
	d = Evaluate(c, NewMatchContext(req, "agent-dev"))
	if d.Action != ActionBlock {
		t.Errorf("dev agent: expected block, got %s", d.Action)
	}
}

func TestEvaluate_HeaderMatch(t *testing.T) {
	src := `
version: 1
name: header-scoped
rules:
  fast:
    - name: block_with_cookie
      action: block
      match:
        headers:
          cookie: ""   # presence check
  default: allow
`
	p, _ := Parse([]byte(src))
	c, _ := Compile(p)

	// No cookie: default allow.
	req := mkReq(t, "GET", "https://example.com/")
	d := Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionAllow {
		t.Errorf("no cookie: expected allow, got %s", d.Action)
	}

	// With cookie: blocked.
	req.Header.Set("Cookie", "sess=x")
	d = Evaluate(c, NewMatchContext(req, ""))
	if d.Action != ActionBlock {
		t.Errorf("with cookie: expected block, got %s", d.Action)
	}
}

func TestCompile_InvalidAction(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
name: bad
rules:
  fast:
    - name: x
      action: burninate
  default: allow
`))
	if err != nil {
		t.Fatalf("parse: %v", err) // parse should succeed; compile should fail
	}
	// Re-parse + compile path.
	p, _ := Parse([]byte(`
version: 1
name: bad
rules:
  fast:
    - name: x
      action: burninate
  default: allow
`))
	if _, err := Compile(p); err == nil {
		t.Error("expected compile error for invalid action")
	}
}

func TestCompile_RejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
name: oops
spam: true
rules:
  default: allow
`))
	if err == nil {
		t.Error("expected parse error for unknown field")
	}
}

func TestCompile_DuplicateRuleName(t *testing.T) {
	p, _ := Parse([]byte(`
version: 1
name: dup
rules:
  fast:
    - name: a
      action: allow
    - name: a
      action: block
  default: allow
`))
	if _, err := Compile(p); err == nil {
		t.Error("expected compile error for duplicate rule name")
	}
}

func TestCompiledPolicy_JSONRoundTrip(t *testing.T) {
	c := compileSample(t)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CompiledPolicy
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Ban.Window != time.Hour {
		t.Errorf("ban.Window round-trip: got %s", back.Ban.Window)
	}
	if back.Ban.Duration != 30*time.Minute {
		t.Errorf("ban.Duration round-trip: got %s", back.Ban.Duration)
	}
	if len(back.Rules) != len(c.Rules) {
		t.Errorf("rules len: got %d want %d", len(back.Rules), len(c.Rules))
	}
}
