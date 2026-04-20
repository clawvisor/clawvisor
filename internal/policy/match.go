package policy

import (
	"net/http"
	"strings"
)

// MatchContext is the evaluated request form passed into Evaluate.
// Pre-extracting fields lets us avoid repeat parsing across rules.
type MatchContext struct {
	Host         string              // lower-cased hostname, no port
	Method       string              // upper-cased method
	Path         string              // URL path including leading slash
	Headers      map[string]string   // lower-cased keys; value == first header value
	Query        map[string][]string // from URL.Query()
	AgentTokenID string              // cvis_... OR agent UUID (caller's choice)
}

// NewMatchContext builds a MatchContext from an HTTP request + agent id.
func NewMatchContext(req *http.Request, agentTokenID string) *MatchContext {
	ctx := &MatchContext{
		AgentTokenID: agentTokenID,
	}
	if req == nil {
		return ctx
	}
	if req.URL != nil {
		ctx.Host = strings.ToLower(req.URL.Hostname())
		ctx.Path = req.URL.Path
		ctx.Query = req.URL.Query()
	}
	ctx.Method = strings.ToUpper(req.Method)
	if req.Header != nil {
		ctx.Headers = make(map[string]string, len(req.Header))
		for k, v := range req.Header {
			if len(v) > 0 {
				ctx.Headers[strings.ToLower(k)] = v[0]
			}
		}
	}
	return ctx
}

// Decision is the output of Evaluate — the matched action and the
// rule that produced it (nil for the default case).
type Decision struct {
	Action Action
	Rule   *CompiledRule // nil when the default action applies
}

// Evaluate walks the rule list top-to-bottom and returns the first
// match. If no rule matches, returns the policy default.
//
// Performance note: this is hot-path on every proxied request. We avoid
// regex and allocations; pattern matching is a byte-wise glob. At the
// volumes we care about (hundreds of req/s, dozens of rules) this is
// well under 1ms. If a bridge ever has thousands of rules we'd
// pre-index by host — not worth the complexity until we see the load.
func Evaluate(c *CompiledPolicy, ctx *MatchContext) Decision {
	if c == nil || ctx == nil {
		return Decision{Action: ActionAllow}
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if ruleMatches(r, ctx) {
			return Decision{Action: r.Action, Rule: r}
		}
	}
	return Decision{Action: c.DefaultAction}
}

func ruleMatches(r *CompiledRule, ctx *MatchContext) bool {
	if len(r.Agents) > 0 && !containsString(r.Agents, ctx.AgentTokenID) {
		return false
	}
	if len(r.Methods) > 0 && !containsString(r.Methods, ctx.Method) {
		return false
	}
	if len(r.Hosts) > 0 && !anyGlobMatch(r.Hosts, ctx.Host) {
		return false
	}
	if len(r.Paths) > 0 && !anyGlobMatch(r.Paths, ctx.Path) {
		return false
	}
	for k, v := range r.Headers {
		got, ok := ctx.Headers[k]
		if !ok {
			return false
		}
		if v != "" && got != v {
			return false
		}
	}
	for k, v := range r.Query {
		vals, ok := ctx.Query[k]
		if !ok || len(vals) == 0 {
			return false
		}
		if v != "" && !anyGlobMatch([]string{v}, vals[0]) {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func anyGlobMatch(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

// globMatch supports:
//   *  — any run of characters (including empty), but not '/'
//   ** — any run of characters, including '/'
//   ?  — any single non-separator character
//
// Matching is case-sensitive by default; callers should lowercase hosts
// before passing them in (NewMatchContext does).
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern == "*" || pattern == "**" {
		return true
	}
	return globMatchBytes([]byte(pattern), []byte(s))
}

// globMatchBytes is an iterative NFA-style matcher — fast enough for
// our 1ms-p99 target without recursion.
func globMatchBytes(pat, s []byte) bool {
	var pi, si int
	var starPi, starSi int
	var doubleStar bool
	starPi = -1
	for si < len(s) {
		if pi < len(pat) {
			// Handle "**" (crosses path separators).
			if pat[pi] == '*' && pi+1 < len(pat) && pat[pi+1] == '*' {
				starPi = pi
				starSi = si
				doubleStar = true
				pi += 2
				continue
			}
			// Handle "*" (does not cross path separators).
			if pat[pi] == '*' {
				starPi = pi
				starSi = si
				doubleStar = false
				pi++
				continue
			}
			if pat[pi] == '?' || pat[pi] == s[si] {
				pi++
				si++
				continue
			}
		}
		// Backtrack to last star if any.
		if starPi >= 0 {
			// Single-star can't cross '/'.
			if !doubleStar && s[starSi] == '/' {
				return false
			}
			starSi++
			pi = starPi + 1
			if doubleStar {
				pi = starPi + 2
			}
			si = starSi
			continue
		}
		return false
	}
	// Consume trailing stars in the pattern.
	for pi < len(pat) {
		if pat[pi] == '*' {
			pi++
			continue
		}
		return false
	}
	return true
}
