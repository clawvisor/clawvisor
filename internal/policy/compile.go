package policy

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Parse reads a policy from its YAML source. Validates structure and
// returns the raw YAMLPolicy for round-trip display; Compile() converts
// to the runtime CompiledPolicy.
func Parse(src []byte) (*YAMLPolicy, error) {
	var p YAMLPolicy
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true) // reject unknown fields → user finds typos early
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: parse YAML: %w", err)
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("policy: unsupported version %d (supported: 1)", p.Version)
	}
	return &p, nil
}

// Compile turns a parsed YAMLPolicy into its runtime form. Validation
// errors here are fatal: unknown action, malformed window, etc.
func Compile(p *YAMLPolicy) (*CompiledPolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("policy: Compile called with nil YAMLPolicy")
	}
	c := &CompiledPolicy{
		BridgeID: p.BridgeID,
		Name:     p.Name,
		Version:  p.Version,
	}

	// Rules in YAML-specified order; first match wins at evaluation.
	seen := map[string]bool{}
	for i, r := range p.Rules.Fast {
		if r.Name == "" {
			return nil, fmt.Errorf("policy: rule[%d] missing name", i)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("policy: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
		action, err := parseAction(r.Action)
		if err != nil {
			return nil, fmt.Errorf("policy: rule %q: %w", r.Name, err)
		}
		c.Rules = append(c.Rules, CompiledRule{
			Name:    r.Name,
			Action:  action,
			Hosts:   normalizeStrings(r.Match.Hosts),
			Methods: upperStrings(r.Match.Methods),
			Paths:   normalizeStrings(r.Match.Paths),
			Headers: lowerKeys(r.Match.Headers),
			Query:   copyMap(r.Match.Query),
			Agents:  normalizeStrings(r.Match.Agents),
			Message: r.Message,
		})
	}

	// Default action falls back to "allow" so an empty-policy is a no-op.
	def := strings.ToLower(strings.TrimSpace(p.Rules.Default))
	if def == "" {
		def = "allow"
	}
	defAction, err := parseAction(def)
	if err != nil {
		return nil, fmt.Errorf("policy: default: %w", err)
	}
	c.DefaultAction = defAction

	c.Judge = CompiledJudge{
		Enabled:   p.Rules.Judge.Enabled,
		Model:     strings.TrimSpace(p.Rules.Judge.Model),
		TimeoutMs: p.Rules.Judge.TimeoutMs,
		OnError:   strings.TrimSpace(p.Rules.Judge.OnError),
	}

	if p.Ban.Enabled {
		window, err := parseDuration(p.Ban.Window, "ban.window")
		if err != nil {
			return nil, err
		}
		dur, err := parseDuration(p.Ban.BanDuration, "ban.ban_duration")
		if err != nil {
			return nil, err
		}
		scope := p.Ban.Scope
		if scope == "" {
			scope = "per_rule"
		}
		if scope != "per_rule" && scope != "per_bridge" {
			return nil, fmt.Errorf("policy: ban.scope must be per_rule or per_bridge, got %q", scope)
		}
		c.Ban = CompiledBan{
			Enabled:       true,
			MaxViolations: p.Ban.MaxViolations,
			Window:        window,
			Duration:      dur,
			Scope:         scope,
		}
		c.Ban.WindowSeconds = int(window / time.Second)
		c.Ban.DurationSeconds = int(dur / time.Second)
		if c.Ban.MaxViolations <= 0 {
			c.Ban.MaxViolations = 3
		}
	}

	return c, nil
}

func parseAction(s string) (Action, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return ActionAllow, nil
	case "block":
		return ActionBlock, nil
	case "flag":
		return ActionFlag, nil
	case "":
		return "", fmt.Errorf("action missing")
	default:
		return "", fmt.Errorf("unknown action %q (must be allow|block|flag)", s)
	}
}

// parseDuration accepts Go-style duration strings ("1h", "30m", "24h").
// Empty string is an error — the caller should pre-default.
func parseDuration(s, field string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("policy: %s is required", field)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("policy: %s = %q: %w", field, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("policy: %s must be positive", field)
	}
	return d, nil
}

func normalizeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func upperStrings(in []string) []string {
	out := normalizeStrings(in)
	for i, s := range out {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func lowerKeys(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
