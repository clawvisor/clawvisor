package policy

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type ToolMatch struct {
	TaskID string
	Item   runtimetasks.ExpectedTool
}

type EgressRequest struct {
	Host    string
	Method  string
	Path    string
	Query   map[string]any
	Body    map[string]any
	Headers map[string]string
}

type EgressMatch struct {
	TaskID string
	Item   runtimetasks.ExpectedEgress
}

func MatchToolCall(tasks []*store.Task, toolName string, input map[string]any) (*ToolMatch, error) {
	var best *ToolMatch
	bestScore := -1
	for _, task := range tasks {
		env, err := runtimetasks.EnvelopeFromTask(task)
		if err != nil {
			return nil, err
		}
		for _, item := range env.ExpectedTools {
			// Tool-name match. Two layers:
			//
			// 1. Case-insensitive equality — handles the model
			//    pattern-matching from documentation and emitting the
			//    lowercase form (`bash` instead of `Bash`).
			// 2. Tool-class equivalence — handles cross-harness aliases
			//    (Claude Code says `Bash`, Codex says `exec_command`;
			//    Claude Code says `Read`, Codex says `read_file`).
			//
			// A task created in a Claude Code session that declares
			// `Bash` should cover the same work in a Codex session
			// that uses `exec_command`. The model doesn't always know
			// which harness it's in; the task does what it semantically
			// said, not what the literal string matched.
			if !toolNamesMatch(item.ToolName, toolName) {
				continue
			}
			if item.InputRegex != "" {
				if ok, err := matchRegexMap(item.InputRegex, input); err != nil || !ok {
					if err != nil {
						return nil, err
					}
					continue
				}
			}
			if !matchShape(item.InputShape, input) {
				continue
			}
			score := toolMatchSpecificity(item)
			if score > bestScore {
				bestScore = score
				best = &ToolMatch{TaskID: task.ID, Item: item}
			}
		}
	}
	return best, nil
}

// toolNamesMatch decides whether a tool-call's actual name matches a
// task's declared `expected_tools` entry. Equivalent names from
// different harnesses share a class — declaring `Bash` in a Claude
// Code session covers a `exec_command` invocation in a Codex session.
// Falls back to case-insensitive equality so the model can use any
// case it wants.
func toolNamesMatch(declared, actual string) bool {
	if strings.EqualFold(declared, actual) {
		return true
	}
	dc := toolClass(declared)
	ac := toolClass(actual)
	return dc != "" && dc == ac
}

// toolClass returns the canonical class for a tool name, or "" when
// the tool isn't part of a known cross-harness alias group. The match
// is case-insensitive.
//
// Adding a new alias is intentionally one-step: append to this map.
// Tools NOT in the map fall through to case-insensitive name equality
// only — no risk of an unrelated tool name picking up an aliased
// class by accident.
func toolClass(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "shell", "exec", "exec_command":
		return "shell"
	case "read", "read_file":
		return "read_file"
	case "edit", "notebookedit", "apply_patch", "edit_file":
		return "edit_file"
	case "write", "write_file":
		return "write_file"
	case "webfetch", "fetch", "http_request", "web_fetch":
		return "web_fetch"
	}
	return ""
}

func MatchEgressRequest(tasks []*store.Task, req EgressRequest) (*EgressMatch, error) {
	host := strings.ToLower(req.Host)
	method := strings.ToUpper(req.Method)
	var best *EgressMatch
	bestScore := -1
	for _, task := range tasks {
		env, err := runtimetasks.EnvelopeFromTask(task)
		if err != nil {
			return nil, err
		}
		for _, item := range env.ExpectedEgress {
			if strings.ToLower(item.Host) != host {
				continue
			}
			if item.Method != "" && strings.ToUpper(item.Method) != method {
				continue
			}
			if item.Path != "" && item.Path != req.Path {
				continue
			}
			if item.PathRegex != "" {
				ok, err := regexp.MatchString(item.PathRegex, req.Path)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			if !matchShape(item.QueryShape, req.Query) || !matchShape(item.BodyShape, req.Body) {
				continue
			}
			if !matchHeaders(item.Headers, req.Headers) {
				continue
			}
			score := egressMatchSpecificity(item)
			if score > bestScore {
				bestScore = score
				best = &EgressMatch{TaskID: task.ID, Item: item}
			}
		}
	}
	return best, nil
}

func matchHeaders(shape map[string]any, headers map[string]string) bool {
	if len(shape) == 0 {
		return true
	}
	lowered := make(map[string]any, len(headers))
	for k, v := range headers {
		lowered[strings.ToLower(k)] = v
	}
	return matchShape(shape, lowered)
}

func matchShape(shape map[string]any, actual map[string]any) bool {
	if len(shape) == 0 {
		return true
	}
	if actual == nil {
		actual = map[string]any{}
	}
	if req, ok := shape["required_keys"].([]any); ok {
		for _, key := range req {
			k, _ := key.(string)
			if k == "" {
				continue
			}
			if _, exists := actual[k]; !exists {
				return false
			}
		}
	}
	if req, ok := shape["required_keys"].([]string); ok {
		for _, k := range req {
			if _, exists := actual[k]; !exists {
				return false
			}
		}
	}
	if forbid, ok := shape["forbid_keys"].([]any); ok {
		for _, key := range forbid {
			k, _ := key.(string)
			if _, exists := actual[k]; exists {
				return false
			}
		}
	}
	if forbid, ok := shape["forbid_keys"].([]string); ok {
		for _, k := range forbid {
			if _, exists := actual[k]; exists {
				return false
			}
		}
	}
	return true
}

func matchRegexMap(expr string, actual map[string]any) (bool, error) {
	if actual == nil {
		return false, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false, err
	}
	if re.MatchString(flattenRegexMap(actual)) {
		return true, nil
	}
	body, err := json.Marshal(actual)
	if err != nil {
		return false, err
	}
	return re.Match(body), nil
}

func flattenRegexMap(actual map[string]any) string {
	if len(actual) == 0 {
		return ""
	}
	keys := make([]string, 0, len(actual))
	for key := range actual {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		lines = appendFlattenedRegexLines(lines, key, actual[key])
	}
	return strings.Join(lines, "\n")
}

func appendFlattenedRegexLines(lines []string, path string, value any) []string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = appendFlattenedRegexLines(lines, path+"."+key, typed[key])
		}
	case []any:
		for i, item := range typed {
			lines = appendFlattenedRegexLines(lines, path+"["+strconv.Itoa(i)+"]", item)
		}
	default:
		body, _ := json.Marshal(typed)
		lines = append(lines, path+"="+string(body))
	}
	return lines
}

func toolMatchSpecificity(item runtimetasks.ExpectedTool) int {
	score := 1
	if item.InputRegex != "" {
		score += 4
	}
	score += shapeSpecificity(item.InputShape)
	return score
}

func egressMatchSpecificity(item runtimetasks.ExpectedEgress) int {
	score := 1
	if item.Method != "" {
		score++
	}
	if item.Path != "" {
		score += 5
	}
	if item.PathRegex != "" {
		score += 4
	}
	score += shapeSpecificity(item.QueryShape)
	score += shapeSpecificity(item.BodyShape)
	score += shapeSpecificity(item.Headers)
	return score
}

func shapeSpecificity(shape map[string]any) int {
	if len(shape) == 0 {
		return 0
	}
	score := 0
	score += listLen(shape["required_keys"]) * 2
	score += listLen(shape["forbid_keys"]) * 2
	return score + len(shape)
}

func listLen(v any) int {
	switch typed := v.(type) {
	case []string:
		return len(typed)
	case []any:
		return len(typed)
	default:
		return 0
	}
}
