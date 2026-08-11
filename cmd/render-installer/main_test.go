package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRendersLocalTemplateForStaging(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run([]string{
		"-claim", "ABCDEFGHIJ",
		"-target", "claude-code",
		"-agent-name", "claude-code-staging-test",
	}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("run exit=%d stderr=%s", exit, stderr.String())
	}
	for _, want := range []string{
		"#!/bin/sh",
		"APP_URL='https://app.staging.clawvisor.com'",
		"LLM_URL='https://llm.staging.clawvisor.com'",
		"AGENT_NAME='claude-code-staging-test'",
		"claim=ABCDEFGHIJ",
		"Every installer invocation creates a new agent token",
		"claude -p --permission-mode auto --verbose",
		`echo "Initial prompt:"`,
		`--output-format stream-json "$CV_DRY_PROMPT"`,
		"cv_claude_transcript",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("rendered installer missing %q", want)
		}
	}
}

func TestRunRejectsInvalidClaim(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run([]string{"-claim", `bad"claim`}, &stdout, &stderr)
	if exit != 2 {
		t.Fatalf("run exit=%d, want 2", exit)
	}
	if !strings.Contains(stderr.String(), "URL-safe claim") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
