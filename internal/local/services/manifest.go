package services

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Service represents a parsed and validated service.yaml.
type Service struct {
	ID          string
	Name        string
	Description string
	Icon        string
	Platform    string
	Type        string // "exec" or "server"
	Dir         string // absolute path to the service directory

	// Server-mode fields.
	Start          []string
	StartupTimeout time.Duration
	Startup        string // "lazy" or "eager"
	HealthCheck    string

	// Shared fields.
	Env        map[string]string
	Headers    map[string]string // Service-level headers (server mode).
	WorkingDir string
	Actions    []Action

	// Source path for error reporting.
	ManifestPath string
}

// Action represents an action within a service.
type Action struct {
	ID          string
	Name        string
	Description string

	// Exec-mode fields.
	Run   []string // parsed argv
	Stdin string

	// Server-mode fields.
	Method     string
	Path       string
	Body       string
	BodyFormat string // "json" or "text"
	Headers    map[string]string

	// Shared.
	Env     map[string]string
	Timeout time.Duration
	Params  []Param
}

// Param represents an action parameter declaration.
type Param struct {
	Name        string
	Description string
	Type        string // "string", "number", "boolean", "json"
	Required    bool
	Default     *string
}

// rawManifest is the YAML structure before validation.
type rawManifest struct {
	Name        string            `yaml:"name"`
	ID          string            `yaml:"id"`
	Description string            `yaml:"description"`
	Icon        string            `yaml:"icon"`
	Platform    string            `yaml:"platform"`
	Type        string            `yaml:"type"`
	Start       interface{}       `yaml:"start"`
	StartupTimeout int            `yaml:"startup_timeout"`
	Startup     string            `yaml:"startup"`
	HealthCheck string            `yaml:"health_check"`
	Env         map[string]string `yaml:"env"`
	Headers     map[string]string `yaml:"headers"`
	WorkingDir  string            `yaml:"working_dir"`
	Actions     []rawAction       `yaml:"actions"`
}

type rawAction struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Run         interface{}       `yaml:"run"`
	Stdin       string            `yaml:"stdin"`
	Method      string            `yaml:"method"`
	Path        string            `yaml:"path"`
	Body        string            `yaml:"body"`
	BodyFormat  string            `yaml:"body_format"`
	Headers     map[string]string `yaml:"headers"`
	Env         map[string]string `yaml:"env"`
	Timeout     string            `yaml:"timeout"`
	Params      []rawParam        `yaml:"params"`
}

type rawParam struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description"`
	Type        string  `yaml:"type"`
	Required    bool    `yaml:"required"`
	Default     *string `yaml:"default"`
}

// ExcludedService represents a service that failed to load.
type ExcludedService struct {
	Path     string `json:"path"`
	Category string `json:"category"` // "invalid", "conflict", "unsupported"
	Error    string `json:"error"`
}

// ParseManifest reads and validates a service.yaml file.
func ParseManifest(path string, defaultTimeout time.Duration) (*Service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	var raw rawManifest
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	if raw.Name == "" {
		return nil, fmt.Errorf("missing required field: name")
	}

	svcType := raw.Type
	if svcType == "" {
		svcType = "exec"
	}
	if svcType != "exec" && svcType != "server" {
		return nil, fmt.Errorf("invalid type: %q (must be exec or server)", svcType)
	}

	// Check platform.
	if raw.Platform != "" && raw.Platform != runtime.GOOS {
		return nil, &PlatformMismatchError{
			Required: raw.Platform,
			Running:  runtime.GOOS,
		}
	}

	dir := filepath.Dir(path)

	svc := &Service{
		ID:           raw.ID, // May be empty; discovery fills in the path-derived ID if so.
		Name:         raw.Name,
		Description:  raw.Description,
		Icon:         raw.Icon,
		Platform:     raw.Platform,
		Type:         svcType,
		Dir:          dir,
		Env:          raw.Env,
		Headers:      raw.Headers,
		ManifestPath: path,
	}

	if svc.Env == nil {
		svc.Env = make(map[string]string)
	}
	if svc.Headers == nil {
		svc.Headers = make(map[string]string)
	}

	// Working directory.
	if raw.WorkingDir != "" && raw.WorkingDir != "." {
		if filepath.IsAbs(raw.WorkingDir) {
			svc.WorkingDir = raw.WorkingDir
		} else {
			svc.WorkingDir = filepath.Join(dir, raw.WorkingDir)
		}
	} else {
		svc.WorkingDir = dir
	}

	// Server-mode fields.
	if svcType == "server" {
		start, err := parseCommand(raw.Start)
		if err != nil {
			return nil, fmt.Errorf("parsing start command: %w", err)
		}
		if len(start) == 0 {
			return nil, fmt.Errorf("server-mode service requires a start command")
		}
		svc.Start = start

		svc.StartupTimeout = 10 * time.Second
		if raw.StartupTimeout > 0 {
			svc.StartupTimeout = time.Duration(raw.StartupTimeout) * time.Second
		}

		svc.Startup = "lazy"
		if raw.Startup == "eager" {
			svc.Startup = "eager"
		}

		svc.HealthCheck = "/health"
		if raw.HealthCheck != "" {
			svc.HealthCheck = raw.HealthCheck
		}
	}

	// Parse actions.
	if len(raw.Actions) == 0 {
		return nil, fmt.Errorf("service must have at least one action")
	}

	seenActions := make(map[string]bool)
	for _, ra := range raw.Actions {
		action, err := parseAction(ra, svcType, dir, defaultTimeout)
		if err != nil {
			return nil, fmt.Errorf("action %q: %w", ra.ID, err)
		}
		if seenActions[action.ID] {
			return nil, fmt.Errorf("duplicate action ID: %q", action.ID)
		}
		seenActions[action.ID] = true
		svc.Actions = append(svc.Actions, *action)
	}

	return svc, nil
}

func parseAction(ra rawAction, svcType, svcDir string, defaultTimeout time.Duration) (*Action, error) {
	if ra.ID == "" {
		return nil, fmt.Errorf("missing required field: id")
	}

	a := &Action{
		ID:          ra.ID,
		Name:        ra.Name,
		Description: ra.Description,
		Stdin:       ra.Stdin,
		Method:      ra.Method,
		Path:        ra.Path,
		Body:        ra.Body,
		Headers:     ra.Headers,
		Env:         ra.Env,
		Timeout:     defaultTimeout,
	}

	if a.Name == "" {
		a.Name = a.ID
	}
	if a.Env == nil {
		a.Env = make(map[string]string)
	}
	if a.Headers == nil {
		a.Headers = make(map[string]string)
	}

	// Body format.
	a.BodyFormat = ra.BodyFormat
	if a.BodyFormat == "" && a.Body != "" {
		a.BodyFormat = "json"
	}
	if a.BodyFormat == "" {
		a.BodyFormat = "text"
	}

	// Timeout.
	if ra.Timeout != "" {
		dur, err := time.ParseDuration(ra.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout %q: %w", ra.Timeout, err)
		}
		a.Timeout = dur
	}

	// Parse params and validate.
	for _, rp := range ra.Params {
		if rp.Name == "" {
			return nil, fmt.Errorf("param missing name")
		}
		if rp.Required && rp.Default != nil {
			return nil, fmt.Errorf("param %q has both required and default", rp.Name)
		}
		p := Param{
			Name:        rp.Name,
			Description: rp.Description,
			Type:        rp.Type,
			Required:    rp.Required,
			Default:     rp.Default,
		}
		if p.Type == "" {
			p.Type = "string"
		}
		a.Params = append(a.Params, p)
	}

	// Type-specific validation.
	if svcType == "exec" {
		run, err := parseCommand(ra.Run)
		if err != nil {
			return nil, fmt.Errorf("parsing run command: %w", err)
		}
		if len(run) == 0 {
			return nil, fmt.Errorf("exec-mode action requires a run command")
		}
		// Resolve relative paths.
		if strings.HasPrefix(run[0], "./") || strings.HasPrefix(run[0], "../") {
			run[0] = filepath.Join(svcDir, run[0])
		}
		a.Run = run
	} else {
		// Server mode.
		if a.Method == "" {
			a.Method = "GET"
		}
		a.Method = strings.ToUpper(a.Method)
		if a.Path == "" {
			return nil, fmt.Errorf("server-mode action requires a path")
		}
	}

	return a, nil
}

func parseCommand(v interface{}) ([]string, error) {
	if v == nil {
		return nil, nil
	}

	switch val := v.(type) {
	case string:
		if val == "" {
			return nil, nil
		}
		return Shlex(val)
	case []interface{}:
		var args []string
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("command array elements must be strings")
			}
			args = append(args, s)
		}
		return args, nil
	default:
		return nil, fmt.Errorf("command must be a string or array of strings")
	}
}

// PlatformMismatchError indicates a service is for a different platform.
type PlatformMismatchError struct {
	Required string
	Running  string
}

func (e *PlatformMismatchError) Error() string {
	return fmt.Sprintf("platform mismatch: requires %s, running %s", e.Required, e.Running)
}
