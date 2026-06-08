package shellpolicy

import (
	"encoding/json"
	"testing"
)

func TestIsShellPollToolRequiresExactlyEmptyChars(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "empty chars is poll", raw: json.RawMessage(`{"chars":""}`), want: true},
		{name: "space is input", raw: json.RawMessage(`{"chars":" "}`), want: false},
		{name: "newline is input", raw: json.RawMessage(`{"chars":"\n"}`), want: false},
		{name: "command text is input", raw: json.RawMessage(`{"chars":"pwd\n"}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsShellPollTool("write_stdin", tt.raw); got != tt.want {
				t.Fatalf("IsShellPollTool = %v, want %v", got, tt.want)
			}
		})
	}
}
