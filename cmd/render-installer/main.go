// Command render-installer renders a shell agent installer from the local
// templates and writes it to stdout. It is a developer tool for exercising
// local installer changes against a remote Clawvisor environment without
// running a second server.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/handlers"
)

var (
	validClaim     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	validAgentName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render-installer", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		appURL         string
		llmURL         string
		target         string
		agentName      string
		claim          string
		claimFromStdin bool
	)
	fs.StringVar(&appURL, "app-url", "https://app.staging.clawvisor.com", "Clawvisor control-plane URL baked into the installer")
	fs.StringVar(&llmURL, "llm-url", "https://llm.staging.clawvisor.com", "LLM proxy URL baked into routed installers")
	fs.StringVar(&target, "target", "claude-code", "installer target: claude-code or codex")
	fs.StringVar(&agentName, "agent-name", "", "agent name baked into the installer (defaults to target)")
	fs.StringVar(&claim, "claim", "", "single-use connection claim")
	fs.BoolVar(&claimFromStdin, "claim-stdin", false, "read the single-use connection claim from stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "render-installer: unexpected positional arguments")
		return 2
	}
	if claimFromStdin {
		if claim != "" {
			fmt.Fprintln(stderr, "render-installer: -claim and -claim-stdin are mutually exclusive")
			return 2
		}
		// Claims are at most 64 URL-safe characters. Two extra bytes allow one
		// terminal newline (LF or CRLF), and the final byte detects overflow; a
		// larger input remains invalid and fails the shape check below.
		claimBytes, err := io.ReadAll(io.LimitReader(stdin, 67))
		if err != nil {
			fmt.Fprintf(stderr, "render-installer: read claim from stdin: %v\n", err)
			return 2
		}
		claim = strings.TrimSuffix(string(claimBytes), "\n")
		claim = strings.TrimSuffix(claim, "\r")
	}

	if !validClaim.MatchString(claim) {
		fmt.Fprintln(stderr, "render-installer: -claim must be a 1-64 character URL-safe claim")
		return 2
	}
	if target != "claude-code" && target != "codex" {
		fmt.Fprintln(stderr, "render-installer: -target must be claude-code or codex")
		return 2
	}
	if agentName == "" {
		agentName = target
	}
	if !validAgentName.MatchString(agentName) {
		fmt.Fprintln(stderr, "render-installer: -agent-name must use 1-64 letters, digits, dots, underscores, or dashes")
		return 2
	}

	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	if !validHTTPBaseURL(appURL) {
		fmt.Fprintln(stderr, "render-installer: -app-url must be an absolute http(s) base URL without a query or fragment")
		return 2
	}
	if llmURL == "" {
		llmURL = appURL
	} else {
		llmURL = strings.TrimRight(strings.TrimSpace(llmURL), "/")
		if !validHTTPBaseURL(llmURL) {
			fmt.Fprintln(stderr, "render-installer: -llm-url must be an absolute http(s) base URL without a query or fragment")
			return 2
		}
	}

	query := url.Values{}
	query.Set("claim", claim)
	query.Set("agent_name", agentName)
	query.Set("route", "skill-only")
	path := "/skill/install/" + target + ".sh?" + query.Encode()

	h := handlers.NewInstallerHandler("", "", false, llmURL, appURL)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body := strings.TrimRight(rec.Body.String(), "\r\n")
		fmt.Fprintf(stderr, "render-installer: render failed with HTTP %d: %s\n", rec.Code, body)
		return 1
	}
	if _, err := io.Copy(stdout, rec.Body); err != nil {
		fmt.Fprintf(stderr, "render-installer: write output: %v\n", err)
		return 1
	}
	return 0
}

func validHTTPBaseURL(raw string) bool {
	// url.Parse accepts both delimiters even when their values are empty
	// (https://host? and https://host#). Appending /api/... to either form
	// produces a syntactically valid but semantically broken endpoint.
	if strings.ContainsAny(raw, "?#") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil &&
		(u.Scheme == "http" || u.Scheme == "https") &&
		u.Host != "" &&
		u.RawQuery == "" &&
		!u.ForceQuery &&
		u.Fragment == ""
}
