package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/clawvisor/clawvisor/internal/local/services"
)

// ExecResult holds the result of an exec-mode action invocation.
type ExecResult struct {
	Success  bool              `json:"success"`
	Data     *ExecData         `json:"data,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// ExecData holds the output data from an exec-mode action.
type ExecData struct {
	Stdout         string            `json:"stdout"`
	StdoutEncoding string            `json:"stdout_encoding,omitempty"`
	Stderr         string            `json:"stderr"`
	StderrEncoding string            `json:"stderr_encoding,omitempty"`
	ExitCode       int               `json:"exit_code"`
	Truncated      map[string]bool   `json:"truncated,omitempty"`
}

// RunExec executes an exec-mode action and returns the result.
func RunExec(
	ctx context.Context,
	svc *services.Service,
	action *services.Action,
	params map[string]string,
	globalEnv map[string]string,
	maxOutputSize int64,
	requestID string,
) *ExecResult {
	// Build resolved params (apply defaults).
	resolved := resolveParams(action, params)

	// Build environment.
	env := buildEnv(svc, action, resolved, globalEnv, requestID)

	// Create command — we manage cancellation manually (SIGTERM then SIGKILL)
	// instead of using exec.CommandContext which sends SIGKILL directly.
	cmdCtx, cancel := context.WithTimeout(ctx, action.Timeout)
	defer cancel()

	cmd := exec.Command(action.Run[0], action.Run[1:]...)
	cmd.Dir = svc.WorkingDir
	cmd.Env = env

	// Handle stdin.
	if action.Stdin != "" {
		interpolated := InterpolateTemplate(action.Stdin, resolved, action.Params, "text")
		cmd.Stdin = strings.NewReader(interpolated)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Set process group so we can kill children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return &ExecResult{
			Success: false,
			Error:   fmt.Sprintf("exec %s.%s: %s", svc.ID, action.ID, err),
		}
	}

	// Watch for context cancellation (timeout or disconnect) and send SIGTERM→SIGKILL.
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- cmd.Wait()
	}()

	var err error
	var cancelled bool
	select {
	case err = <-doneCh:
		// Process exited normally (or with error).
	case <-cmdCtx.Done():
		// Context cancelled (timeout or WebSocket disconnect).
		// Send SIGTERM, then SIGKILL after 3s grace period.
		cancelled = true
		killProcessGroup(cmd, doneCh, 3*time.Second)
		err = <-doneCh // Wait for process to actually exit.
	}

	// Process output.
	outResult := ProcessOutput(stdout.Bytes(), maxOutputSize)
	errResult := ProcessOutput(stderr.Bytes(), maxOutputSize)

	if cancelled {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return &ExecResult{
				Success: false,
				Error:   fmt.Sprintf("timed out after %s", action.Timeout),
			}
		}
		return &ExecResult{
			Success: false,
			Error:   "cancelled (connection closed)",
		}
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResult{
				Success: false,
				Error:   fmt.Sprintf("exec %s.%s: %s", svc.ID, action.ID, err),
			}
		}
	}

	data := &ExecData{
		Stdout:   outResult.Data,
		Stderr:   errResult.Data,
		ExitCode: exitCode,
	}

	if outResult.Encoding != "" {
		data.StdoutEncoding = outResult.Encoding
	}
	if errResult.Encoding != "" {
		data.StderrEncoding = errResult.Encoding
	}

	truncated := make(map[string]bool)
	if outResult.Truncated {
		truncated["stdout"] = true
	}
	if errResult.Truncated {
		truncated["stderr"] = true
	}
	if len(truncated) > 0 {
		data.Truncated = truncated
	}

	success := exitCode == 0
	var errMsg string
	if !success {
		errMsg = errResult.Data
	}

	return &ExecResult{
		Success: success,
		Data:    data,
		Error:   errMsg,
	}
}

func resolveParams(action *services.Action, provided map[string]string) map[string]string {
	resolved := make(map[string]string, len(action.Params))
	for _, p := range action.Params {
		if v, ok := provided[p.Name]; ok {
			resolved[p.Name] = v
		} else if p.Default != nil {
			resolved[p.Name] = *p.Default
		} else {
			resolved[p.Name] = ""
		}
	}
	return resolved
}

func buildEnv(svc *services.Service, action *services.Action, params map[string]string, globalEnv map[string]string, requestID string) []string {
	// Start with system environment.
	env := os.Environ()

	// Add global env.
	for k, v := range globalEnv {
		env = append(env, k+"="+v)
	}

	// Service-level env (with template interpolation).
	for k, v := range svc.Env {
		v = InterpolateTemplate(v, params, action.Params, "text")
		env = append(env, k+"="+v)
	}

	// Action-level env (with template interpolation).
	for k, v := range action.Env {
		v = InterpolateTemplate(v, params, action.Params, "text")
		env = append(env, k+"="+v)
	}

	// PARAM_* env vars.
	for name, value := range params {
		envName := "PARAM_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		env = append(env, envName+"="+value)
	}

	// Magic variables.
	env = append(env,
		"CLAWVISOR_SERVICE_ID="+svc.ID,
		"CLAWVISOR_ACTION_ID="+action.ID,
		"CLAWVISOR_SERVICE_DIR="+svc.Dir,
		"CLAWVISOR_REQUEST_ID="+requestID,
	)

	return env
}

// killProcessGroup sends SIGTERM to the process group, then SIGKILL after the
// grace period. It does NOT call Wait — the caller is responsible for waiting
// via cmd.Wait() to avoid double-Wait races.
func killProcessGroup(cmd *exec.Cmd, doneCh <-chan error, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-doneCh:
		// Process exited after SIGTERM.
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
