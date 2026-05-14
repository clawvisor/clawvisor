package llmproxy

import (
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type approvalBodyEditor interface {
	LatestApprovalReply() (verb, approvalID string, ok bool)
	ReplaceLatestUserText(expectedVerb, replacement string) ([]byte, bool, error)
}

func newApprovalBodyEditor(req *http.Request, provider conversation.Provider, body []byte) (approvalBodyEditor, bool) {
	switch provider {
	case conversation.ProviderAnthropic:
		return anthropicApprovalBodyEditor{body: body}, true
	case conversation.ProviderOpenAI:
		if conversation.IsOpenAIChatCompletionsEndpoint(req) {
			return openAIChatApprovalBodyEditor{body: body}, true
		}
		return openAIResponsesApprovalBodyEditor{body: body}, true
	default:
		return nil, false
	}
}

func approvalReplyFromBody(req *http.Request, provider conversation.Provider, body []byte) (verb, approvalID string) {
	editor, ok := newApprovalBodyEditor(req, provider, body)
	if !ok {
		return "", ""
	}
	verb, approvalID, ok = editor.LatestApprovalReply()
	if !ok {
		return "", ""
	}
	return verb, approvalID
}

type anthropicApprovalBodyEditor struct {
	body []byte
}

func (e anthropicApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	verb, approvalID := conversation.AnthropicApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e anthropicApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, replacement string) ([]byte, bool, error) {
	return replaceAnthropicApprovalReply(e.body, expectedVerb, replacement)
}

type openAIChatApprovalBodyEditor struct {
	body []byte
}

func (e openAIChatApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	verb, approvalID := conversation.OpenAIApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e openAIChatApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, replacement string) ([]byte, bool, error) {
	return replaceOpenAIChatApprovalReply(e.body, expectedVerb, replacement)
}

type openAIResponsesApprovalBodyEditor struct {
	body []byte
}

func (e openAIResponsesApprovalBodyEditor) LatestApprovalReply() (string, string, bool) {
	verb, approvalID := conversation.OpenAIApprovalReply(e.body)
	return verb, approvalID, verb != ""
}

func (e openAIResponsesApprovalBodyEditor) ReplaceLatestUserText(expectedVerb, replacement string) ([]byte, bool, error) {
	return replaceOpenAIResponsesApprovalReply(e.body, expectedVerb, replacement)
}
