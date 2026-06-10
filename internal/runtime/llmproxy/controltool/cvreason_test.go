package controltool

import (
	"strings"
	"testing"
)

func TestControlNoticeInstructsAgentToIncludeCvReason(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"Bash", "Read", "Edit", "Write"})
	if !strings.Contains(notice, "PER-CALL `cvreason`") {
		t.Fatalf("notice missing cvreason section; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Clawvisor strips `cvreason`") {
		t.Fatalf("notice should tell the agent that cvreason is stripped before the tool runs; got:\n%s", notice)
	}
	if !strings.Contains(notice, "intent verification") {
		t.Fatalf("notice should explain that cvreason feeds intent verification; got:\n%s", notice)
	}
}
