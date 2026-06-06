package llmproxy

import (
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptrecognition"
)

// ScriptSessionPrefix is the leading byte sequence of every token
// produced by Mint. The resolver middleware uses it to distinguish
// script-session tokens from one-shot nonces.
const ScriptSessionPrefix = scriptrecognition.ScriptSessionPrefix

func ScriptSessionToolUse(input json.RawMessage, resolverBaseURL string) bool {
	return scriptrecognition.ScriptSessionToolUse(input, resolverBaseURL)
}

func NormalizeScriptSessionPathPrefix(raw string) (string, error) {
	return scriptrecognition.NormalizeScriptSessionPathPrefix(raw)
}

func ScriptSessionPathPrefixMatch(prefix, requestPath string) bool {
	return scriptrecognition.ScriptSessionPathPrefixMatch(prefix, requestPath)
}
