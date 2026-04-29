package policy

import (
	"encoding/json"
	"regexp"
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
			return &ToolMatch{TaskID: task.ID, Item: item}, nil
		}
	}
	return nil, nil
}

func MatchEgressRequest(tasks []*store.Task, req EgressRequest) (*EgressMatch, error) {
	host := strings.ToLower(req.Host)
	method := strings.ToUpper(req.Method)
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
			return &EgressMatch{TaskID: task.ID, Item: item}, nil
		}
	}
	return nil, nil
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
	body, err := json.Marshal(actual)
	if err != nil {
		return false, err
	}
	return regexp.MatchString(expr, string(body))
}
