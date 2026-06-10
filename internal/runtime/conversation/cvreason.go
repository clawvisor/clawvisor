package conversation

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// CvReasonField is the input-JSON key the control notice instructs agents
// to include on every tool_use to explain WHY the call fits the approved
// task scope. The parser strips it before forwarding so the client never
// sees it; intent verification reads it from ToolUse.CvReason.
const CvReasonField = "cvreason"

// ExtractCvReason parses input as a JSON object, removes the cvreason
// key, and returns the extracted reason and the stripped bytes. If input
// is empty, isn't a JSON object, or has no cvreason key, the original
// input is returned with an empty reason and ok=false — callers should
// keep the original Input untouched in that case (preserving byte
// fidelity for inputs we couldn't reshape).
func ExtractCvReason(input json.RawMessage) (reason string, stripped json.RawMessage, ok bool) {
	if len(input) == 0 {
		return "", input, false
	}
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", input, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return "", input, false
	}
	raw, present := fields[CvReasonField]
	if !present {
		return "", input, false
	}
	delete(fields, CvReasonField)
	if err := json.Unmarshal(raw, &reason); err != nil {
		reason = strings.TrimSpace(string(raw))
	}
	if !utf8.ValidString(reason) {
		reason = strings.ToValidUTF8(reason, "")
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return "", input, false
	}
	return reason, json.RawMessage(out), true
}

// PopulateCvReason returns a copy of tu with cvreason extracted from
// Input into CvReason. Prefer NewToolUseFromInput at construction sites;
// PopulateCvReason exists for the occasional case where the ToolUse was
// already built and we need to normalize it post-hoc.
func PopulateCvReason(tu ToolUse) ToolUse {
	reason, stripped, ok := ExtractCvReason(tu.Input)
	if !ok {
		return tu
	}
	tu.CvReason = reason
	tu.Input = stripped
	return tu
}

// NewToolUseFromInput is the canonical constructor for a ToolUse built
// from upstream-emitted input bytes. It extracts cvreason from the input
// JSON, populates CvReason, and stores the stripped bytes in Input — so
// every site that takes raw upstream bytes flows through one normalizer
// instead of relying on per-site PopulateCvReason wrapping. New
// construction sites in the response parsers should use this
// constructor; raw ToolUse{...} literals leak cvreason to the client.
func NewToolUseFromInput(id string, index int, name string, input json.RawMessage) ToolUse {
	return PopulateCvReason(ToolUse{
		ID:    id,
		Index: index,
		Name:  name,
		Input: input,
	})
}
