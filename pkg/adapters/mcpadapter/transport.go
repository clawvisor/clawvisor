package mcpadapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
)

// Transport opens a fresh MCP session and returns a Caller — the unified
// surface that hides whether we're talking to a local subprocess, an
// in-process goroutine (tests), or a remote HTTP endpoint.
type Transport interface {
	Open(ctx context.Context, env map[string]string) (mcpclient.Caller, error)
}

// ── stdio (subprocess) ──────────────────────────────────────────────────────

// StdioTransport spawns a subprocess and pipes stdio. This is how nearly
// every published MCP server (notion-mcp-server, github-mcp, etc.) runs
// locally — via npx, uvx, or a compiled binary.
type StdioTransport struct {
	Command []string
}

func (t *StdioTransport) Open(ctx context.Context, env map[string]string) (mcpclient.Caller, error) {
	if len(t.Command) == 0 {
		return nil, fmt.Errorf("stdio transport: empty command")
	}
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...)
	// Inherit the parent environment so PATH, HOME, etc. resolve (npx, node,
	// shell stubs, etc. depend on these). Caller-supplied env appends after,
	// so duplicate keys override the inherited value.
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newStreamCaller(&subprocConn{stdin: stdin, stdout: stdout, cmd: cmd}), nil
}

type subprocConn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (c *subprocConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *subprocConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *subprocConn) Close() error {
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

// ── in-process (tests) ──────────────────────────────────────────────────────

// InProcessTransport runs an MCP server handler in a goroutine connected by
// pipes. Used in tests so we don't compile and exec a separate binary.
type InProcessTransport struct {
	Serve func(ctx context.Context, in io.Reader, out io.Writer, env map[string]string) error
}

func (t *InProcessTransport) Open(ctx context.Context, env map[string]string) (mcpclient.Caller, error) {
	if t.Serve == nil {
		return nil, fmt.Errorf("in-process transport: Serve handler is nil")
	}
	clientToServer, serverIn := io.Pipe()
	serverOut, clientFromServer := io.Pipe()

	go func() {
		err := t.Serve(ctx, clientToServer, clientFromServer, env)
		_ = clientFromServer.CloseWithError(err)
		_ = clientToServer.CloseWithError(err)
	}()

	return newStreamCaller(&pipeConn{PipeReader: serverOut, PipeWriter: serverIn}), nil
}

type pipeConn struct {
	*io.PipeReader
	*io.PipeWriter
}

func (p *pipeConn) Close() error {
	_ = p.PipeReader.Close()
	_ = p.PipeWriter.Close()
	return nil
}

// ── HTTP (remote, vendor-hosted MCP) ────────────────────────────────────────

// HTTPTransport speaks MCP "streamable HTTP" against a remote endpoint —
// the production-shape transport for vendor-hosted MCP servers. No local
// install, no subprocess, no node/npm in the Clawvisor container.
//
// Auth: the credential bytes flow into a request header on every call.
// Default is the Bearer pattern; YAML specs can override the header name
// and prefix to accommodate vendors using something else.
type HTTPTransport struct {
	Endpoint     string
	HeaderName   string // defaults to "Authorization"
	HeaderPrefix string // defaults to "Bearer "
	HTTPClient   *http.Client // optional; nil → use default
}

// httpTokenEnvKey is the conventional env-map slot the MCPAdapter populates
// with the extracted credential. Both stdio (via spec.credential_env) and
// http (via this key) read from the same env map; the HTTP transport just
// promotes whatever the spec's CredentialEnv set into a header.
const httpTokenEnvKey = "__mcp_http_token__"

func (t *HTTPTransport) Open(_ context.Context, env map[string]string) (mcpclient.Caller, error) {
	if t.Endpoint == "" {
		return nil, fmt.Errorf("http transport: endpoint is required")
	}
	headerName := t.HeaderName
	if headerName == "" {
		headerName = "Authorization"
	}
	prefix := t.HeaderPrefix
	if prefix == "" {
		prefix = "Bearer "
	}
	headers := map[string]string{}
	if tok := env[httpTokenEnvKey]; tok != "" {
		headers[headerName] = prefix + tok
	}
	return mcpclient.NewHTTP(t.Endpoint, headers, t.HTTPClient), nil
}
