package llmproxy

import (
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
)

type SyntheticApprovalHistoryStripRequest = historystrip.SyntheticApprovalHistoryStripRequest
type SyntheticApprovalHistoryStripResult = historystrip.SyntheticApprovalHistoryStripResult
type SecretDecisionHistoryStripRequest = historystrip.SecretDecisionHistoryStripRequest
type SecretDecisionHistoryStripResult = historystrip.SecretDecisionHistoryStripResult

const ToolApprovalSubstitutedPromptMarker = historystrip.ToolApprovalSubstitutedPromptMarker

func StripSyntheticApprovalHistory(req SyntheticApprovalHistoryStripRequest) (SyntheticApprovalHistoryStripResult, error) {
	return historystrip.StripSyntheticApprovalHistory(req)
}

func StripSecretDecisionHistory(req SecretDecisionHistoryStripRequest) (SecretDecisionHistoryStripResult, error) {
	return historystrip.StripSecretDecisionHistory(req)
}
