package historystrip

import (
	"encoding/json"
	"regexp"
	"strings"
)

const SecretDecisionIDMarker = "[clawvisor:secret="
const InlineApprovalSubstitutedPromptMarker = "Clawvisor wants to create a task to cover this work:"

// InlineExpansionApprovalSubstitutedPromptMarker is the expansion-side
// counterpart to InlineApprovalSubstitutedPromptMarker. Both prompts
// land in assistant text the historystrip uses to identify a
// substituted approval turn (so the next bare-verb user reply and the
// AskUserQuestion tool_use pair can both be removed before forwarding
// to the upstream model). Without an expansion-specific marker the
// strip would skip the expansion's assistant turn, leave the
// AskUserQuestion tool_use in place, and then orphan the rewritten
// user turn (Anthropic 400: "tool use concurrency issues"). Keep this
// in sync with renderExpansionApprovalPrompt's opening line.
const InlineExpansionApprovalSubstitutedPromptMarker = "Clawvisor wants to expand the scope of an existing task"

const InlineTaskNoticeOpenPrefix = `<clawvisor-notice kind="task-`

// SyntheticToolUseIDPrefix is the namespace the inline-approval
// intercept stamps on every tool_use_id it synthesizes for a
// substituted picker call. The strip path uses this prefix to find
// orphan tool_results (whose parent tool_use was just stripped from
// the assistant turn) without needing to know the specific harness
// tool name — keeping this package harness-agnostic. Producers and
// consumers must agree on the prefix; both reference this constant.
const SyntheticToolUseIDPrefix = "toolu_clawvisor_"

type SecretDecisionAction string

const (
	SecretDecisionNone      SecretDecisionAction = ""
	SecretDecisionAllowOnce SecretDecisionAction = "allow_once"
	SecretDecisionDiscard   SecretDecisionAction = "discard"
	SecretDecisionNotSecret SecretDecisionAction = "not_secret"
	SecretDecisionVault     SecretDecisionAction = "vault"
)

type SecretDecisionReply struct {
	Action    SecretDecisionAction
	VaultName string
}

func ParseSecretDecisionReply(text string) SecretDecisionReply {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Trim(normalized, "`\"' ")
	switch {
	case normalized == "allow once" || normalized == "allow":
		return SecretDecisionReply{Action: SecretDecisionAllowOnce}
	case normalized == "discard" || normalized == "redact" || normalized == "discard secret" || normalized == "redact secret":
		return SecretDecisionReply{Action: SecretDecisionDiscard}
	case normalized == "not secret" || normalized == "not a secret" || normalized == "this is not a secret":
		return SecretDecisionReply{Action: SecretDecisionNotSecret}
	case strings.HasPrefix(normalized, "vault "):
		name := strings.TrimSpace(normalized[len("vault "):])
		if strings.HasPrefix(name, "as ") {
			name = strings.TrimSpace(name[len("as "):])
		}
		return SecretDecisionReply{Action: SecretDecisionVault, VaultName: SanitizeVaultName(name)}
	default:
		return SecretDecisionReply{}
	}
}

func ContainsInlineApprovalAugmentationMarker(text string) bool {
	return strings.Contains(text, InlineTaskNoticeOpenPrefix)
}

func SanitizeVaultName(value string) string {
	value = NormalizeSecretLabel(value)
	if value == "" {
		return "secret"
	}
	if len(value) > 96 {
		value = value[:96]
	}
	return value
}

func NormalizeSecretLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = regexp.MustCompile(`[^a-z0-9._:-]+`).ReplaceAllString(value, "_")
	return strings.Trim(value, "._:-")
}

func flattenAnthropicTaskReplyText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func flattenOpenAITaskReplyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		return simple
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
