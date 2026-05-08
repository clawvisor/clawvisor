package llmproxy

import (
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
)

// yamlDefiner is implemented by adapters whose definition can be exposed —
// currently only *yamlruntime.YAMLAdapter. We accept anything that satisfies
// the contract so that future Go-only adapters could opt in.
type yamlDefiner interface {
	Def() yamldef.ServiceDef
}

// ResolvedAction is the (service, action) tuple a (host, method, path) maps to.
// PathTemplate is the original YAML template (e.g. "/repos/{{.owner}}/{{.repo}}/issues")
// preserved so callers can show the human-readable form in audit + approval UI.
type ResolvedAction struct {
	ServiceID    string
	ActionID     string
	Method       string
	PathTemplate string
}

// ServiceCatalog reverse-resolves an outgoing HTTP request — (host, method, path)
// — to a Clawvisor (service, action) pair using the YAML adapter definitions.
//
// This is the bridge between the lite-proxy inspector (which knows what URL
// the agent intends to hit) and the policy/task-scope layer (which only
// understands service IDs + action names).
//
// Resolution is host-first, then path-template-matched. Among multiple action
// matches we pick the most specific by static-segment count — so
// `/repos/{{.o}}/{{.r}}/issues` beats `/repos/{{.o}}/{{.r}}/{{.x}}` for path
// `/repos/x/y/issues`. Method must match exactly.
type ServiceCatalog struct {
	entries []catalogEntry
}

type catalogEntry struct {
	serviceID    string
	actionID     string
	method       string
	host         string
	pathTemplate string
	pathRegex    *regexp.Regexp
	staticScore  int
}

var (
	templateVarRE = regexp.MustCompile(`\{\{\s*\.[A-Za-z0-9_]+\s*\}\}`)
)

// NewServiceCatalog builds a catalog from the loaded YAML service definitions.
// Definitions with non-REST APIs, missing base URLs, or template-driven hosts
// (e.g. `{{.workspace}}.example.com`) are silently skipped — the lite-proxy
// will simply not be able to resolve those hosts back to (service, action),
// and policy will fall through to whatever default it applies for unknown
// destinations.
func NewServiceCatalog(defs []yamldef.ServiceDef) *ServiceCatalog {
	c := &ServiceCatalog{entries: make([]catalogEntry, 0, len(defs)*8)}
	for _, def := range defs {
		if !strings.EqualFold(strings.TrimSpace(def.API.Type), "rest") {
			continue
		}
		host := hostFromBaseURL(def.API.BaseURL)
		if host == "" {
			continue
		}
		serviceID := def.Service.ID
		for actionID, action := range def.Actions {
			if action.Method == "" || action.Path == "" {
				continue
			}
			re, score, ok := compilePathTemplate(action.Path)
			if !ok {
				continue
			}
			c.entries = append(c.entries, catalogEntry{
				serviceID:    serviceID,
				actionID:     actionID,
				method:       strings.ToUpper(action.Method),
				host:         strings.ToLower(host),
				pathTemplate: action.Path,
				pathRegex:    re,
				staticScore:  score,
			})
		}
	}
	return c
}

// Resolve returns the (service, action) for an outgoing request, or false
// when no entry matches. Both the host and the method are required;
// trailing slashes on the path are normalized away. Query strings, if
// supplied, are stripped before matching.
func (c *ServiceCatalog) Resolve(host, method, path string) (ResolvedAction, bool) {
	if c == nil || len(c.entries) == 0 {
		return ResolvedAction{}, false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	method = strings.ToUpper(strings.TrimSpace(method))
	if host == "" || method == "" {
		return ResolvedAction{}, false
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// Strip a single trailing slash unless the path is just "/".
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimRight(path, "/")
	}

	bestScore := -1
	var best ResolvedAction
	found := false
	for _, e := range c.entries {
		if e.host != host || e.method != method {
			continue
		}
		if !e.pathRegex.MatchString(path) {
			continue
		}
		if e.staticScore > bestScore {
			bestScore = e.staticScore
			best = ResolvedAction{
				ServiceID:    e.serviceID,
				ActionID:     e.actionID,
				Method:       e.method,
				PathTemplate: e.pathTemplate,
			}
			found = true
		}
	}
	if !found {
		return ResolvedAction{}, false
	}
	return best, true
}

// hostFromBaseURL extracts the host (excluding port) from a base_url. If the
// URL contains an unresolved template like `{{.workspace}}.example.com`, the
// host has a `{{` substring and we return "" so the catalog skips this def
// — it can't be reverse-mapped without instance-specific config.
func hostFromBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" || strings.Contains(base, "{{") {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// compilePathTemplate converts a YAML path template like
// `/repos/{{.owner}}/{{.repo}}/issues` into an anchored regex matching
// concrete request paths. Each `{{.X}}` becomes `[^/]+`. Returns the
// regex, a static-segment score (count of literal slash-separated
// segments — used for specificity tiebreaking) and ok=false if the
// template fails to compile.
func compilePathTemplate(template string) (*regexp.Regexp, int, bool) {
	if !strings.HasPrefix(template, "/") {
		template = "/" + template
	}
	// Compute static score: count slash-separated segments that don't
	// contain a `{{` placeholder.
	staticScore := 0
	for _, seg := range strings.Split(template, "/") {
		if seg == "" {
			continue
		}
		if !strings.Contains(seg, "{{") {
			staticScore++
		}
	}
	// Build regex by quoting fixed text and replacing template vars.
	var b strings.Builder
	b.WriteString(`^`)
	last := 0
	for _, m := range templateVarRE.FindAllStringIndex(template, -1) {
		b.WriteString(regexp.QuoteMeta(template[last:m[0]]))
		b.WriteString(`[^/]+`)
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(template[last:]))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, 0, false
	}
	return re, staticScore, true
}

// NewServiceCatalogFromRegistry builds a catalog by introspecting the
// shared adapter registry. Only YAML-driven REST adapters contribute
// entries; Go-only adapters (iMessage, SQL) and GraphQL adapters are
// silently skipped — the catalog is purely for HTTP path/method
// reverse-mapping.
func NewServiceCatalogFromRegistry(reg *adapters.Registry) *ServiceCatalog {
	if reg == nil {
		return NewServiceCatalog(nil)
	}
	defs := make([]yamldef.ServiceDef, 0)
	for _, a := range reg.All() {
		// Prefer the *yamlruntime.YAMLAdapter exact type; fall back to the
		// duck-typed interface so future adapter shapes can opt in.
		if ya, ok := a.(*yamlruntime.YAMLAdapter); ok {
			defs = append(defs, ya.Def())
			continue
		}
		if d, ok := a.(yamlDefiner); ok {
			defs = append(defs, d.Def())
		}
	}
	return NewServiceCatalog(defs)
}

// LazyServiceCatalog is a thread-safe wrapper that builds a ServiceCatalog
// the first time Resolve is called. Useful when the caller has a hot path
// that should not pay the build cost until first use, and wants to swap
// in updated definitions without restarting.
type LazyServiceCatalog struct {
	mu      sync.RWMutex
	defs    []yamldef.ServiceDef
	built   *ServiceCatalog
	dirty   bool
}

// NewLazyServiceCatalog returns a lazy catalog seeded with defs.
func NewLazyServiceCatalog(defs []yamldef.ServiceDef) *LazyServiceCatalog {
	return &LazyServiceCatalog{defs: defs, dirty: true}
}

// SetDefinitions replaces the underlying definitions and forces a rebuild
// on the next Resolve call.
func (l *LazyServiceCatalog) SetDefinitions(defs []yamldef.ServiceDef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.defs = defs
	l.dirty = true
	l.built = nil
}

// Resolve mirrors ServiceCatalog.Resolve.
func (l *LazyServiceCatalog) Resolve(host, method, path string) (ResolvedAction, bool) {
	l.mu.RLock()
	if !l.dirty && l.built != nil {
		c := l.built
		l.mu.RUnlock()
		return c.Resolve(host, method, path)
	}
	l.mu.RUnlock()

	l.mu.Lock()
	if l.dirty || l.built == nil {
		l.built = NewServiceCatalog(l.defs)
		l.dirty = false
	}
	c := l.built
	l.mu.Unlock()
	return c.Resolve(host, method, path)
}
