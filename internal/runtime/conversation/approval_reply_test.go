package conversation

import "testing"

func TestParseApprovalReplyText(t *testing.T) {
	tests := []struct {
		input    string
		wantVerb string
		wantID   string
	}{
		{"yes", "approve", ""},
		{"y", "approve", ""},
		{"no", "deny", ""},
		{"n", "deny", ""},
		{"approve", "approve", ""},
		{"deny", "deny", ""},
		{"ok", "", ""},
		{"cancel", "", ""},
		{"approve cv-abc123def456", "approve", "cv-abc123def456"},
		{"deny cv-abc123def456", "deny", "cv-abc123def456"},
		{"", "", ""},
		{"I'll approve it later", "", ""}, //should NOT trigger
		{"looks good to me!", "", ""},     //should NOT trigger
	}
	for _, tt := range tests {
		verb, id := ParseApprovalReplyText(tt.input)
		if verb != tt.wantVerb || id != tt.wantID {
			t.Errorf("ParseApprovalReplyText(%q)=%q,%q, want %q,%q", tt.input, verb, id, tt.wantVerb, tt.wantID)
		}
	}
}
