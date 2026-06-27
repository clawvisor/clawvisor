package llmproxy

import (
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	ControlSyntheticHost  = controltool.ControlSyntheticHost
	ControlSyntheticPath  = controltool.ControlSyntheticPath
	ControlAPIPath        = controltool.ControlAPIPath
	ControlNoticeSentinel = controltool.ControlNoticeSentinel
)

type ControlCall = controltool.ControlCall

func ControlNotice(controlBaseURL string, availableTools []string) string {
	return controltool.ControlNotice(controlBaseURL, availableTools)
}

func ControlNoticeWithPolicy(controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule) string {
	return controltool.ControlNoticeWithPolicy(controlBaseURL, availableTools, toolRules)
}

func InjectControlNotice(provider conversation.Provider, body []byte, controlBaseURL string, availableTools []string) ([]byte, bool, error) {
	return controltool.InjectControlNotice(provider, body, controlBaseURL, availableTools)
}

func InjectControlNoticeWithPolicy(provider conversation.Provider, body []byte, controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule) ([]byte, bool, error) {
	return controltool.InjectControlNoticeWithPolicy(provider, body, controlBaseURL, availableTools, toolRules)
}

func InjectControlNoticeWithSnapshot(provider conversation.Provider, body []byte, controlBaseURL string, availableTools []string, toolRules []*store.RuntimePolicyRule, activeTasksSnapshot string) ([]byte, bool, error) {
	return controltool.InjectControlNoticeWithSnapshot(provider, body, controlBaseURL, availableTools, toolRules, activeTasksSnapshot)
}

func RewriteControlToolUse(t conversation.ToolUse, controlBaseURL string, callerToken string, conversationID string) ([]byte, inspector.Verdict, bool, error) {
	return controltool.RewriteControlToolUse(t, controlBaseURL, callerToken, conversationID)
}

func RewriteControlFailureToolUse(t conversation.ToolUse, controlBaseURL string, callerToken string, reason string) ([]byte, bool, error) {
	return controltool.RewriteControlFailureToolUse(t, controlBaseURL, callerToken, reason)
}

func ParseControlToolUse(t conversation.ToolUse) (ControlCall, bool) {
	return controltool.ParseControlToolUse(t)
}

func ParseControlToolUseWithBase(t conversation.ToolUse, controlBaseURL string) (ControlCall, bool) {
	return controltool.ParseControlToolUseWithBase(t, controlBaseURL)
}

func ControlToolUseMentionsEndpoint(t conversation.ToolUse, controlBaseURL string) bool {
	return controltool.ControlToolUseMentionsEndpoint(t, controlBaseURL)
}
