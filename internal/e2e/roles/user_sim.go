package roles

import (
	"context"
	"fmt"
	"strings"
)

const userSimSystem = `You are a user-sim driving an end-to-end test of an agent runtime.

Your job:
- Pursue the goal below by talking to the agent in plain English.
- Keep messages concise — one to three sentences.
- When the agent has clearly accomplished the goal (or definitively failed),
  output the literal token <DONE> on its own line and stop.

Do NOT issue API calls yourself. The agent runs them. Stay in user voice.

Goal:
%s

Persona context:
%s
`

// UserSim drives the user side of the conversation. It runs on a smaller,
// faster model and stops once it emits <DONE> or hits the turn budget.
type UserSim struct {
	Client   *Client
	Goal     string
	Persona  string
	MaxTurns int
}

// FirstMessage returns the user's opening turn — the message the responder
// first sees.
func (u *UserSim) FirstMessage(ctx context.Context) (string, error) {
	resp, err := u.Client.Send(ctx, Request{
		System: u.systemPrompt(),
		Messages: []Message{
			{Role: "user", Content: "Begin the conversation. Send the first message to the agent."},
		},
		MaxTokens: 256,
	})
	if err != nil {
		return "", fmt.Errorf("user-sim first message: %w", err)
	}
	return strings.TrimSpace(resp.FirstText()), nil
}

// Reply produces the next user-sim turn given the current transcript-from-
// the-user-perspective. The responder transcript needs to be remapped: the
// agent's reply becomes a user message to the user-sim.
func (u *UserSim) Reply(ctx context.Context, fromUserPerspective []Message) (text string, done bool, err error) {
	resp, err := u.Client.Send(ctx, Request{
		System:    u.systemPrompt(),
		Messages:  fromUserPerspective,
		MaxTokens: 256,
	})
	if err != nil {
		return "", false, fmt.Errorf("user-sim reply: %w", err)
	}
	text = strings.TrimSpace(resp.FirstText())
	if strings.Contains(text, "<DONE>") {
		text = strings.TrimSpace(strings.ReplaceAll(text, "<DONE>", ""))
		return text, true, nil
	}
	return text, false, nil
}

func (u *UserSim) systemPrompt() string {
	persona := strings.TrimSpace(u.Persona)
	if persona == "" {
		persona = "(no persona specified)"
	}
	return fmt.Sprintf(userSimSystem, u.Goal, persona)
}
