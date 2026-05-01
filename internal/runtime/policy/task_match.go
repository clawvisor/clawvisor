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
			if item.ToolName != toolName {
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
