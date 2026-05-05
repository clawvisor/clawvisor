package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type sessionToolDefaults struct {
	StarterProfile   string   `json:"starter_profile"`
	WorkingDir       string   `json:"working_dir"`
	ToolAllowedRoots []string `json:"tool_allowed_roots"`
}

func allowSessionScopedToolDefault(session *store.RuntimeSession, toolName string, input map[string]any) (string, bool) {
	defaults := sessionToolDefaultsFromSession(session)
	if !isCodingStarterProfile(defaults.StarterProfile) {
		return "", false
	}
	switch normalizeToolName(toolName) {
	case "read", "read_file", "mcp__filesystem__read_file":
		return allowFileToolInRoots(defaults, toolName, input, "read")
	case "write", "edit", "notebookedit", "write_file", "edit_file", "mcp__filesystem__write_file", "mcp__filesystem__edit_file":
		return allowFileToolInRoots(defaults, toolName, input, "write")
	case "glob", "grep", "ls":
		return allowSearchToolInRoots(defaults, toolName, input)
	default:
		return "", false
	}
}

func sessionToolDefaultsFromSession(session *store.RuntimeSession) sessionToolDefaults {
	var defaults sessionToolDefaults
	if session == nil || len(session.MetadataJSON) == 0 {
		return defaults
	}
	_ = json.Unmarshal(session.MetadataJSON, &defaults)
	defaults.StarterProfile = strings.ToLower(strings.TrimSpace(defaults.StarterProfile))
	defaults.WorkingDir = cleanAbsolutePath(defaults.WorkingDir)
	roots := make([]string, 0, len(defaults.ToolAllowedRoots))
	seen := map[string]bool{}
	for _, root := range defaults.ToolAllowedRoots {
		root = cleanAbsolutePath(root)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	if len(roots) == 0 && defaults.WorkingDir != "" {
		roots = append(roots, defaults.WorkingDir)
	}
	defaults.ToolAllowedRoots = roots
	return defaults
}

func isCodingStarterProfile(profile string) bool {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "claude_code", "codex":
		return true
	default:
		return false
	}
}

func normalizeToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func allowFileToolInRoots(defaults sessionToolDefaults, toolName string, input map[string]any, mode string) (string, bool) {
	paths := referencedToolPaths(defaults, input)
	if len(paths) == 0 {
		return "", false
	}
	for _, p := range paths {
		if !pathWithinAnyRoot(p, defaults.ToolAllowedRoots) {
			return "", false
		}
	}
	action := "read"
	if mode == "write" {
		action = "modify"
	}
	return fmt.Sprintf("coding default: %s %s inside the current workspace or /tmp", action, summarizePathList(paths)), true
}

func allowSearchToolInRoots(defaults sessionToolDefaults, toolName string, input map[string]any) (string, bool) {
	paths := referencedToolPaths(defaults, input)
	if len(paths) == 0 {
		return "coding default: search inside the current workspace", true
	}
	for _, p := range paths {
		if !pathWithinAnyRoot(p, defaults.ToolAllowedRoots) {
			return "", false
		}
	}
	return fmt.Sprintf("coding default: search %s inside the current workspace or /tmp", summarizePathList(paths)), true
}

func referencedToolPaths(defaults sessionToolDefaults, input map[string]any) []string {
	if len(input) == 0 {
		return nil
	}
	paths := []string{}
	add := func(value string) {
		if resolved := resolveToolPath(defaults.WorkingDir, value); resolved != "" {
			paths = append(paths, resolved)
		}
	}
	for _, key := range []string{"file_path", "path", "directory", "cwd"} {
		if value, ok := stringInput(input, key); ok {
			add(value)
		}
	}
	for _, key := range []string{"paths", "file_paths"} {
		if values, ok := stringSliceInput(input, key); ok {
			for _, value := range values {
				add(value)
			}
		}
	}
	if len(paths) == 0 {
		if pattern, ok := stringInput(input, "pattern"); ok && strings.HasPrefix(strings.TrimSpace(pattern), "/") {
			add(globPrefix(pattern))
		}
	}
	return dedupePaths(paths)
}

func resolveToolPath(workingDir, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if workingDir == "" {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(workingDir, value))
}

func cleanAbsolutePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if abs, err := filepath.Abs(value); err == nil {
		value = abs
	}
	return filepath.Clean(value)
}

func pathWithinAnyRoot(path string, roots []string) bool {
	path = cleanAbsolutePath(path)
	if path == "" {
		return false
	}
	for _, root := range roots {
		root = cleanAbsolutePath(root)
		if root == "" {
			continue
		}
		if path == root {
			return true
		}
		if strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func dedupePaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func stringInput(input map[string]any, key string) (string, bool) {
	value, ok := input[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func stringSliceInput(input map[string]any, key string) ([]string, bool) {
	value, ok := input[key]
	if !ok {
		return nil, false
	}
	switch typed := value.(type) {
	case []string:
		return typed, len(typed) > 0
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, _ := item.(string)
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, text)
			}
		}
		return out, len(out) > 0
	default:
		return nil, false
	}
}

func globPrefix(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}
	for idx, r := range pattern {
		switch r {
		case '*', '?', '[', '{':
			if idx == 0 {
				return ""
			}
			return strings.TrimRight(pattern[:idx], string(os.PathSeparator))
		}
	}
	return pattern
}

func summarizePathList(paths []string) string {
	if len(paths) == 0 {
		return "the requested path"
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return fmt.Sprintf("%s and %d other path(s)", paths[0], len(paths)-1)
}

func summarizeToolUse(toolName string, input map[string]any) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "tool call"
	}
	switch normalizeToolName(toolName) {
	case "read", "read_file", "mcp__filesystem__read_file":
		if path, ok := stringInput(input, "file_path"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
		if path, ok := stringInput(input, "path"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
	case "write", "edit", "notebookedit", "write_file", "edit_file", "mcp__filesystem__write_file", "mcp__filesystem__edit_file":
		if path, ok := stringInput(input, "file_path"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
		if path, ok := stringInput(input, "path"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
	case "glob", "grep", "ls":
		if path, ok := stringInput(input, "path"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
		if path, ok := stringInput(input, "directory"); ok {
			return fmt.Sprintf("%s %s", name, path)
		}
		if pattern, ok := stringInput(input, "pattern"); ok {
			return fmt.Sprintf("%s %s", name, pattern)
		}
	case "bash", "mcp__shell__exec":
		if command, ok := stringInput(input, "command"); ok {
			return fmt.Sprintf("%s %s", name, command)
		}
	}
	return name
}
