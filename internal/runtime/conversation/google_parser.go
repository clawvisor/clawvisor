package conversation

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GoogleParser recognizes Google Gemini API requests at
// generativelanguage.googleapis.com and extracts turns from the
// contents[] array (Gemini's analog to Anthropic's messages[]).
//
// Production traffic for Google requires more than this parser —
// stream codecs, forwarder routing, response shape handling. The
// stub validates that adding a new provider through the conversation
// package boundary requires no edits to policies/ or pipeline/.
type GoogleParser struct{}

// Name returns ProviderGoogle.
func (GoogleParser) Name() Provider { return ProviderGoogle }

// Matches returns true for Gemini API generateContent endpoints
// (both the synchronous :generateContent and streaming
// :streamGenerateContent variants).
func (GoogleParser) Matches(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(hostFromRequest(req))
	if host != "generativelanguage.googleapis.com" {
		return false
	}
	if !strings.Contains(req.URL.Path, "/v1") {
		return false
	}
	return strings.Contains(req.URL.Path, ":generateContent") || strings.Contains(req.URL.Path, ":streamGenerateContent")
}

type googleRequest struct {
	Contents []googleContent `json:"contents"`
	// systemInstruction is Gemini's analog of Anthropic's system field.
	SystemInstruction *googleContent `json:"systemInstruction,omitempty"`
}

type googleContent struct {
	Role  string              `json:"role"`
	Parts []googleContentPart `json:"parts"`
}

type googleContentPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *googleFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *googleFunctionResponse `json:"functionResponse,omitempty"`
}

type googleFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type googleFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// ParseRequest extracts turns from a Gemini request body. Maps
// Gemini's role/parts shape to the canonical Turn/Role vocabulary:
//   - role "user" → RoleUser
//   - role "model" → RoleAssistant
//   - functionResponse parts in user role → RoleTool
//   - systemInstruction → RoleSystem (synthesized as a leading turn)
//
// Tool calls (functionCall parts) appear as inline text inside the
// assistant turn, prefixed with "<tool_use name=...>" — same convention
// the Anthropic and OpenAI parsers use.
func (GoogleParser) ParseRequest(body []byte) ([]Turn, error) {
	var r googleRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(r.Contents)+1)
	if r.SystemInstruction != nil {
		if sys := flattenGoogleParts(r.SystemInstruction.Parts); sys != "" {
			out = append(out, Turn{Role: RoleSystem, Content: sys})
		}
	}
	for _, c := range r.Contents {
		content, role := googleContentToTurn(c)
		if content == "" {
			continue
		}
		out = append(out, Turn{Role: role, Content: content})
	}
	return out, nil
}

func googleContentToTurn(c googleContent) (content string, role Role) {
	// Default role mapping: Gemini uses "user" and "model".
	role = RoleUser
	if c.Role == "model" {
		role = RoleAssistant
	}
	// If any part carries a functionResponse, the turn is a tool result.
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			role = RoleTool
			break
		}
	}
	content = flattenGoogleParts(c.Parts)
	return content, role
}

func flattenGoogleParts(parts []googleContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
			b.WriteByte('\n')
		}
		if p.FunctionCall != nil {
			b.WriteString("<tool_use name=")
			b.WriteString(p.FunctionCall.Name)
			if len(p.FunctionCall.Args) > 0 {
				b.WriteString(" input=")
				b.Write(p.FunctionCall.Args)
			}
			b.WriteByte('>')
			b.WriteByte('\n')
		}
		if p.FunctionResponse != nil {
			b.WriteString(strings.TrimSpace(string(p.FunctionResponse.Response)))
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}
