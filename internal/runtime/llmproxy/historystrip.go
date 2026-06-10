package llmproxy

import (
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
)

type SyntheticApprovalHistoryStripRequest = historystrip.SyntheticApprovalHistoryStripRequest
type SyntheticApprovalHistoryStripResult = historystrip.SyntheticApprovalHistoryStripResult
type SecretDecisionHistoryStripRequest = historystrip.SecretDecisionHistoryStripRequest
type SecretDecisionHistoryStripResult = historystrip.SecretDecisionHistoryStripResult
type SecretDecisionAction = historystrip.SecretDecisionAction
type SecretDecisionReply = historystrip.SecretDecisionReply

const ToolApprovalSubstitutedPromptMarker = historystrip.ToolApprovalSubstitutedPromptMarker
const InlineApprovalSubstitutedPromptMarker = historystrip.InlineApprovalSubstitutedPromptMarker
const SecretDecisionIDMarker = historystrip.SecretDecisionIDMarker
const inlineTaskNoticeOpenPrefix = historystrip.InlineTaskNoticeOpenPrefix

// SyntheticToolUseIDPrefix re-exports the historystrip-side constant
// the inline-approval intercept stamps on every synthesized tool_use.
// Lives here so producers in llmproxy don't need to import the
// historystrip subpackage just to namespace an ID. See
// historystrip.SyntheticToolUseIDPrefix for the contract details.
const SyntheticToolUseIDPrefix = historystrip.SyntheticToolUseIDPrefix

const (
	SecretDecisionNone      = historystrip.SecretDecisionNone
	SecretDecisionAllowOnce = historystrip.SecretDecisionAllowOnce
	SecretDecisionDiscard   = historystrip.SecretDecisionDiscard
	SecretDecisionNotSecret = historystrip.SecretDecisionNotSecret
	SecretDecisionVault     = historystrip.SecretDecisionVault
)

func StripSyntheticApprovalHistory(req SyntheticApprovalHistoryStripRequest) (SyntheticApprovalHistoryStripResult, error) {
	return historystrip.StripSyntheticApprovalHistory(req)
}

func StripSecretDecisionHistory(req SecretDecisionHistoryStripRequest) (SecretDecisionHistoryStripResult, error) {
	return historystrip.StripSecretDecisionHistory(req)
}

func ParseSecretDecisionReply(text string) SecretDecisionReply {
	return historystrip.ParseSecretDecisionReply(text)
}

func NormalizeSecretLabel(value string) string {
	return historystrip.NormalizeSecretLabel(value)
}

func SanitizeVaultName(value string) string {
	return historystrip.SanitizeVaultName(value)
}

func ContainsInlineApprovalAugmentationMarker(text string) bool {
	return historystrip.ContainsInlineApprovalAugmentationMarker(text)
}

func normalizeSecretLabel(value string) string {
	return NormalizeSecretLabel(value)
}

func sanitizeVaultName(value string) string {
	return SanitizeVaultName(value)
}

func containsInlineApprovalAugmentationMarker(text string) bool {
	return ContainsInlineApprovalAugmentationMarker(text)
}
