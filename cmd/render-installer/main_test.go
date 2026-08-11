package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunRendersLocalTemplateForStaging(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run([]string{
		"-claim", "ABCDEFGHIJ",
		"-target", "claude-code",
		"-agent-name", "claude-code-staging-test",
	}, strings.NewReader(""), &stdout, &stderr)
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
	exit := run([]string{"-claim", `bad"claim`}, strings.NewReader(""), &stdout, &stderr)
	if exit != 2 {
		t.Fatalf("run exit=%d, want 2", exit)
	}
	if !strings.Contains(stderr.String(), "URL-safe claim") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestRunReadsClaimFromStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run([]string{
		"-claim-stdin",
		"-target", "codex",
		"-agent-name", "codex-staging-test",
	}, strings.NewReader("ABCDEFGHIJ\r\n"), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("run exit=%d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claim=ABCDEFGHIJ") {
		t.Fatalf("rendered installer did not contain stdin claim")
	}
}

func TestRunRejectsAmbiguousClaimSources(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := run(
		[]string{"-claim", "ABCDEFGHIJ", "-claim-stdin"},
		strings.NewReader("OTHERCLAIM\n"),
		&stdout,
		&stderr,
	)
	if exit != 2 {
		t.Fatalf("run exit=%d, want 2", exit)
	}
	if !strings.HasSuffix(stderr.String(), "\n") ||
		!strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestRunRejectsInvalidStdinClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim string
	}{
		{name: "empty", claim: ""},
		{name: "invalid character", claim: "bad claim\n"},
		{name: "multiple lines", claim: "ABCDEFGHIJ\nSECOND\n"},
		{name: "too long", claim: strings.Repeat("a", 65) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := run(
				[]string{"-claim-stdin"},
				strings.NewReader(tc.claim),
				&stdout,
				&stderr,
			)
			if exit != 2 {
				t.Fatalf("run exit=%d, want 2", exit)
			}
			if stdout.Len() != 0 ||
				!strings.HasSuffix(stderr.String(), "\n") ||
				!strings.Contains(stderr.String(), "URL-safe claim") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestValidHTTPBaseURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{url: "https://app.staging.clawvisor.com", want: true},
		{url: "http://localhost:25297/prefix", want: true},
		{url: "https://example.com/path%3Fpart%23part", want: true},
		{url: "https://example.com/';id;'", want: true},
		{url: "https://example.com?mode=staging", want: false},
		{url: "https://example.com?", want: false},
		{url: "https://example.com#fragment", want: false},
		{url: "https://example.com#", want: false},
		{url: "ftp://example.com", want: false},
		{url: "/relative", want: false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := validHTTPBaseURL(tc.url); got != tc.want {
				t.Errorf("validHTTPBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestRunRejectsQueryAndFragmentBaseURLs(t *testing.T) {
	for _, tc := range []struct {
		flag string
		url  string
	}{
		{flag: "-app-url", url: "https://example.com?mode=staging"},
		{flag: "-app-url", url: "https://example.com#"},
		{flag: "-llm-url", url: "https://example.com?"},
		{flag: "-llm-url", url: "https://example.com#proxy"},
	} {
		t.Run(tc.flag+tc.url, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := run(
				[]string{"-claim", "ABCDEFGHIJ", tc.flag, tc.url},
				strings.NewReader(""),
				&stdout,
				&stderr,
			)
			if exit != 2 {
				t.Fatalf("run exit=%d, want 2", exit)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "without a query or fragment") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestStagingScriptUsesUniqueNamesAndKeepsClaimOutOfGoProcess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	fakeBin := t.TempDir()
	writeTestExecutable(t, filepath.Join(fakeBin, "date"), "#!/bin/sh\nprintf '20260811123456\\n'\n")
	writeTestExecutable(t, filepath.Join(fakeBin, "go"), `#!/bin/sh
printf 'ARGV=%s\n' "$*"
printf 'CLAIM_ENV=%s\n' "${CLAIM-unset}"
printf 'ENV_CLAIM_ENV=%s\n' "${ENV_CLAIM-unset}"
printf 'RENDER_CLAIM_ENV=%s\n' "${RENDER_CLAIM-unset}"
IFS= read -r claim || claim=""
printf 'STDIN=%s\n' "$claim"
`)

	const claim = "SINGLE_USE_CLAIM"
	var names []string
	namePattern := regexp.MustCompile(`-agent-name (codex-staging-test-20260811123456-[0-9]+)`)
	for i := 0; i < 2; i++ {
		cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "render-staging-installer.sh"), "codex")
		cmd.Env = []string{
			"CLAIM=" + claim,
			"HOME=" + t.TempDir(),
			"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run %d: %v\n%s", i+1, err, out)
		}
		text := string(out)
		if strings.Contains(strings.SplitN(text, "\n", 2)[0], claim) ||
			!strings.Contains(text, "-claim-stdin") ||
			!strings.Contains(text, "CLAIM_ENV=unset") ||
			!strings.Contains(text, "ENV_CLAIM_ENV=unset") ||
			!strings.Contains(text, "RENDER_CLAIM_ENV=unset") ||
			!strings.Contains(text, "STDIN="+claim) {
			t.Fatalf("claim channel was not isolated:\n%s", text)
		}
		match := namePattern.FindStringSubmatch(text)
		if match == nil {
			t.Fatalf("default agent name lacks timestamp and PID:\n%s", text)
		}
		names = append(names, match[1])
	}
	if names[0] == names[1] {
		t.Fatalf("rapid renders reused agent name %q", names[0])
	}

	t.Run("stdin without trailing newline", func(t *testing.T) {
		cmd := exec.Command(
			"sh",
			filepath.Join(repoRoot, "scripts", "render-staging-installer.sh"),
			"--claim-stdin",
			"codex",
		)
		cmd.Stdin = strings.NewReader(claim)
		cmd.Env = []string{
			"HOME=" + t.TempDir(),
			"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("render from unterminated stdin claim: %v\n%s", err, out)
		}
		text := string(out)
		if !strings.Contains(text, "-claim-stdin") ||
			!strings.Contains(text, "CLAIM_ENV=unset") ||
			!strings.Contains(text, "STDIN="+claim) {
			t.Fatalf("unterminated stdin claim was not preserved:\n%s", text)
		}
	})
}

func writeTestExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
