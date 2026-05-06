package onedrive

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

func TestExecute_InvalidToken(t *testing.T) {
	a := New()
	_, err := a.Execute(context.Background(), adapters.Request{
		Action:     "list_files",
		Credential: []byte(`{"invalid": true}`),
	})
	if err == nil {
		t.Errorf("Expected error for invalid token, got nil")
	}
}

func TestExecute_UnsupportedAction(t *testing.T) {
	a := New()
	_, err := a.Execute(context.Background(), adapters.Request{
		Action:     "unknown_action",
		Credential: []byte(`{"token": "token123"}`),
	})
	if err == nil {
		t.Errorf("Expected error for unsupported action, got nil")
	}
}
