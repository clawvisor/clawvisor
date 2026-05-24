package conversation

import "testing"

func TestFindLatestApprovalIDMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"none", "no marker here", ""},
		{"single", "blah blah [clawvisor:approval=cv-abcdefghij12]", "cv-abcdefghij12"},
		{"picks last of two", "first [clawvisor:approval=cv-aaaaaaaaaaaa] then [clawvisor:approval=cv-bbbbbbbbbbbb]", "cv-bbbbbbbbbbbb"},
		{"long form", "[clawvisor:approval=cv-abcdefghijklmnopqrstuvwxyz]", "cv-abcdefghijklmnopqrstuvwxyz"},
		{"malformed unclosed", "[clawvisor:approval=cv-abc no bracket", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindLatestApprovalIDMarker(tt.in)
			if got != tt.want {
				t.Errorf("FindLatestApprovalIDMarker(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

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
