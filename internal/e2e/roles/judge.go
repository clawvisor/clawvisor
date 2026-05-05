package roles

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const judgeSystem = `You are a strict judge grading an end-to-end agent test.

You will be given a list of soft expectations and a transcript. For each
expectation, decide whether the transcript clearly satisfies it.

Reply with JSON only:
{"results":[{"expectation":"<verbatim>","pass":true|false,"reason":"<one short sentence>"}]}

Default to pass=false when the evidence is ambiguous.`

// JudgeResult is one rubric item the judge scored.
type JudgeResult struct {
	Expectation string `json:"expectation"`
	Pass        bool   `json:"pass"`
	Reason      string `json:"reason"`
}

// JudgeOutput is the full rubric pass.
type JudgeOutput struct {
	Results []JudgeResult `json:"results"`
}

// Judge runs the soft-expectations rubric over the transcript. The
// transcript is rendered to a plain text view first because the judge
// doesn't need raw tool blocks. Returns the per-expectation results.
func Judge(ctx context.Context, c *Client, soft []string, transcript []Message) (*JudgeOutput, error) {
	if len(soft) == 0 {
		return &JudgeOutput{}, nil
	}
	body := buildJudgePrompt(soft, transcript)
	resp, err := c.Send(ctx, Request{
		System: judgeSystem,
		Messages: []Message{
			{Role: "user", Content: body},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	text := strings.TrimSpace(resp.FirstText())
	// The model sometimes wraps JSON in a fenced block. Strip fences.
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var out JudgeOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("judge: parse %q: %w", text, err)
	}
	return &out, nil
}

func buildJudgePrompt(soft []string, transcript []Message) string {
	var sb strings.Builder
	sb.WriteString("Soft expectations to grade:\n")
	for i, s := range soft {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, s)
	}
	sb.WriteString("\nTranscript:\n")
	for _, m := range transcript {
		fmt.Fprintf(&sb, "[%s]\n", m.Role)
		switch v := m.Content.(type) {
		case string:
			sb.WriteString(v)
		case []ContentBlock:
			for _, b := range v {
				switch b.Type {
				case "text":
					sb.WriteString(b.Text)
				case "tool_use":
					fmt.Fprintf(&sb, "<tool_use name=%q input=%s />", b.Name, string(b.Input))
				case "tool_result":
					fmt.Fprintf(&sb, "<tool_result is_error=%v>%v</tool_result>", b.IsError, b.Content)
				}
			}
		default:
			enc, _ := json.Marshal(v)
			sb.Write(enc)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}
