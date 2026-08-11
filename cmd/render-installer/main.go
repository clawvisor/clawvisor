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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render-installer", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		appURL    string
		llmURL    string
		target    string
		agentName string
		claim     string
	)
	fs.StringVar(&appURL, "app-url", "https://app.staging.clawvisor.com", "Clawvisor control-plane URL baked into the installer")
	fs.StringVar(&llmURL, "llm-url", "https://llm.staging.clawvisor.com", "LLM proxy URL baked into routed installers")
	fs.StringVar(&target, "target", "claude-code", "installer target: claude-code or codex")
	fs.StringVar(&agentName, "agent-name", "", "agent name baked into the installer (defaults to target)")
	fs.StringVar(&claim, "claim", "", "single-use connection claim")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "render-installer: unexpected positional arguments")
		return 2
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
	if !validHTTPURL(appURL) {
		fmt.Fprintln(stderr, "render-installer: -app-url must be an absolute http(s) URL")
		return 2
	}
	if llmURL == "" {
		llmURL = appURL
	} else {
		llmURL = strings.TrimRight(strings.TrimSpace(llmURL), "/")
		if !validHTTPURL(llmURL) {
			fmt.Fprintln(stderr, "render-installer: -llm-url must be an absolute http(s) URL")
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
		fmt.Fprintf(stderr, "render-installer: render failed with HTTP %d: %s", rec.Code, rec.Body.String())
		return 1
	}
	if _, err := io.Copy(stdout, rec.Body); err != nil {
		fmt.Fprintf(stderr, "render-installer: write output: %v\n", err)
		return 1
	}
	return 0
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
