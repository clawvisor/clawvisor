package conversation

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ConversationIDMarker is the prefix of the parseable footer the proxy
// embeds in the first assistant turn on harnesses without a native
// session identifier (today: OpenAI Chat Completions only). Format:
// "[clawvisor:conversation=cv-conv-<id>]". Lives in this package so
// both the request-body parser and the prompt renderer agree on the
// literal.
//
// The marker is a distinct key ("conversation") from the approval
// marker ("approval"), so a parser keyed on the marker name cannot
// confuse them. The cv-conv-<id> prefix on the value is a human-
// readability nicety, not a parser-level disambiguator.
const ConversationIDMarker = "[clawvisor:conversation="

// ConversationIDPrefix is the prefix every minted conversation ID
// carries. Kept narrow so the scanner regex can reject any value that
// doesn't structurally look like a Clawvisor-minted ID.
const ConversationIDPrefix = "cv-conv-"

// conversationIDMarkerRE matches the full bracket-enclosed marker
// shape on Chat Completions assistant turns. The inner [a-z0-9]+ is a
// tolerant superset of the actual mint alphabet (lowercase base32 =
// a-z2-7) so a future mint-format tweak doesn't have to update this
// regex in lockstep.
var conversationIDMarkerRE = regexp.MustCompile(`\[clawvisor:conversation=(` + regexp.QuoteMeta(ConversationIDPrefix) + `[a-z0-9]+)\]`)

// conversationIDRandRead is the entropy source for NewConversationID,
// hooked at the package level so tests can inject a deterministic
// reader. Matches the pattern used by liteApprovalRandRead in the
// llmproxy package.
var conversationIDRandRead = rand.Read

// NewConversationID mints a fresh conversation identifier used by the
// inject-and-echo flow on harnesses without a native session ID. The
// returned value has the form "cv-conv-<26 chars lowercase base32>",
// matching the entropy and encoding shape of approval IDs.
//
// The mint is a pure function — no I/O, no clock dependency — so the
// handler can call it inline when it decides to mint on turn 1.
func NewConversationID() (string, error) {
	var b [16]byte
	if _, err := conversationIDRandRead(b[:]); err != nil {
		return "", fmt.Errorf("generate conversation id: %w", err)
	}
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	return ConversationIDPrefix + encoded, nil
}

// RenderConversationIDMarker returns the bracketed marker form that
// callers embed verbatim in the rendered assistant text. Centralized
// so the prepend renderer and the parser agree on the literal.
func RenderConversationIDMarker(id string) string {
	return ConversationIDMarker + id + "]"
}

// FindInjectedConversationID returns the conversation ID from the
// rightmost [clawvisor:conversation=cv-conv-...] marker found in an
// assistant-role message of the inbound request body. Scoped to:
//
//  1. ASSISTANT-ROLE turns only, walked structurally — a user typing
//     the marker text into their own prompt MUST NOT hijack a
//     conversation under an attacker-controlled ID.
//  2. OpenAI Chat Completions inbound shape only, since that is the
//     only provider+endpoint where the proxy injects the marker.
//     Other providers / endpoints return "" without parsing.
//
// Returns "" when no marker is found, the body fails to parse, or the
// provider/endpoint isn't a Chat Completions request. Most-recent-wins
// when multiple assistant turns each carry a marker (compaction may
// keep a marker from an earlier turn, but the freshest is the one
// produced by the latest mint).
func FindInjectedConversationID(req *http.Request, provider Provider, body []byte) string {
	if len(body) == 0 || provider != ProviderOpenAI {
		return ""
	}
	if req == nil || !isOpenAIChatCompletionsEndpoint(req) {
		return ""
	}
	var probe struct {
		Messages []openAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	latest := ""
	for _, msg := range probe.Messages {
		if msg.Role != "assistant" {
			continue
		}
		text := flattenOpenAIContent(msg.Content)
		if text == "" {
			continue
		}
		matches := conversationIDMarkerRE.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			continue
		}
		// Most-recent-wins within an assistant turn AND across turns:
		// overwrite latest as we walk forward, so the final value is
		// the last marker in the last assistant turn that carried one.
		latest = strings.ToLower(matches[len(matches)-1][1])
	}
	return latest
}
