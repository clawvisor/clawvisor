package roles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// httpRequestTool is the single tool the responder is given. It mirrors a
// simplified curl: name a method, URL, headers, and an optional body, get
// back the upstream response.
var httpRequestTool = Tool{
	Name:        "http_request",
	Description: "Issue an HTTP(S) request through the runtime proxy. Use this for every external API call.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"method": {"type": "string", "description": "HTTP method, e.g. GET or POST"},
			"url":    {"type": "string", "description": "Absolute URL"},
			"headers": {"type": "object", "additionalProperties": {"type": "string"}},
			"body":   {"type": "string", "description": "Request body (use a JSON-encoded string for JSON payloads)"}
		},
		"required": ["method", "url"]
	}`),
}

// ResponderConfig configures one responder turn loop.
type ResponderConfig struct {
	Client       *Client
	System       string
	HTTPClient   *http.Client // wired to use the runtime proxy with Proxy-Authorization
	MaxTurns     int
	MaxToolCalls int
	Logf         func(string, ...any) // nil-safe verbose logger
}

// ResponderResult is what the responder returns at the end of a run.
type ResponderResult struct {
	FinalText  string
	ToolCalls  int
	Transcript []Message
}

// RunResponder drives the responder until it stops calling tools (the model
// emits a plain text turn) or budgets are exhausted. The user message it
// starts from is appended to messages by the caller.
func RunResponder(ctx context.Context, cfg ResponderConfig, messages []Message) (*ResponderResult, error) {
	if cfg.Client == nil || cfg.HTTPClient == nil {
		return nil, fmt.Errorf("responder: client and http client are required")
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}
	maxToolCalls := cfg.MaxToolCalls
	if maxToolCalls <= 0 {
		maxToolCalls = 16
	}
	out := &ResponderResult{Transcript: append([]Message(nil), messages...)}

	for turn := 0; turn < maxTurns; turn++ {
		resp, err := cfg.Client.Send(ctx, Request{
			System:   cfg.System,
			Messages: out.Transcript,
			Tools:    []Tool{httpRequestTool},
		})
		if err != nil {
			return out, fmt.Errorf("responder turn %d: %w", turn, err)
		}
		out.Transcript = append(out.Transcript, Message{Role: "assistant", Content: resp.Content})

		if text := resp.FirstText(); text != "" {
			logf(cfg.Logf, "\nagent» %s\n", oneLine(text))
		}

		if resp.StopReason != "tool_use" {
			out.FinalText = resp.FirstText()
			return out, nil
		}

		var toolResults []ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" || block.Name != httpRequestTool.Name {
				continue
			}
			out.ToolCalls++
			if out.ToolCalls > maxToolCalls {
				logf(cfg.Logf, "tool» BUDGET EXHAUSTED (%d > %d)", out.ToolCalls, maxToolCalls)
				toolResults = append(toolResults, ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					IsError:   true,
					Content:   "tool budget exhausted",
				})
				continue
			}
			result, isErr, summary := executeHTTPRequest(ctx, cfg.HTTPClient, block.Input)
			logf(cfg.Logf, "tool» %s", summary)
			toolResults = append(toolResults, ContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ID,
				IsError:   isErr,
				Content:   result,
			})
		}
		out.Transcript = append(out.Transcript, Message{Role: "user", Content: toolResults})
	}
	return out, fmt.Errorf("responder: turn budget %d exhausted", maxTurns)
}

// executeHTTPRequest returns (toolResultJSON, isError, oneLineSummary).
func executeHTTPRequest(ctx context.Context, client *http.Client, input json.RawMessage) (string, bool, string) {
	var args struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		msg := fmt.Sprintf("invalid tool input: %s", err.Error())
		return msg, true, msg
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if args.Body != "" {
		bodyReader = strings.NewReader(args.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, args.URL, bodyReader)
	if err != nil {
		msg := fmt.Sprintf("%s %s — build request: %s", method, args.URL, err.Error())
		return msg, true, msg
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		msg := fmt.Sprintf("%s %s — error: %s", method, args.URL, err.Error())
		return msg, true, msg
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	out := map[string]any{
		"status":  resp.StatusCode,
		"headers": flattenHeaders(resp.Header),
		"body":    string(body),
	}
	encoded, _ := json.Marshal(out)
	summary := fmt.Sprintf("%s %s → %d (%d bytes)", method, args.URL, resp.StatusCode, len(body))
	return string(encoded), false, summary
}

func logf(fn func(string, ...any), format string, args ...any) {
	if fn == nil {
		return
	}
	fn(format, args...)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
