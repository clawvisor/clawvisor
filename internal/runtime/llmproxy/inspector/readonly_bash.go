package inspector

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// IsReadOnlyBashCommand reports whether cmd is a shell pipeline /
// command composed entirely of read-only commands with no writes,
// subshells, or command substitution. Used by the postprocess
// decision-gate to let routine reads (pwd, ls, cat, grep, …) run
// without requiring an approved task scope.
//
// Returns (true, "") for read-only. Returns (false, reason) when the
// command mutates state, escapes via substitution, or uses a
// command we don't recognize. The reason is suitable for surfacing
// in audit rows; it never includes user-supplied substrings that
// might inject shell metacharacters.
//
// Conservative by construction: a command we don't know about is
// refused. The cost of a false-negative (a safe command we don't
// recognize) is "user has to approve it"; the cost of a false-positive
// (an unsafe command we mistakenly allow) is "skipped scope gate."
func IsReadOnlyBashCommand(cmd string) (bool, string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false, "empty command"
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return false, "parse error"
	}
	if len(file.Stmts) == 0 {
		return false, "no statements"
	}
	if len(file.Stmts) > 1 {
		return false, "multiple statements"
	}
	stmt := file.Stmts[0]
	if stmt.Negated || stmt.Background || stmt.Coprocess {
		return false, "negated, backgrounded, or coprocess"
	}

	var (
		unsafeReason string
		callExprs    []*syntax.CallExpr
	)
	syntax.Walk(file, func(node syntax.Node) bool {
		if unsafeReason != "" || node == nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CmdSubst:
			unsafeReason = "command substitution"
			return false
		case *syntax.ProcSubst:
			unsafeReason = "process substitution"
			return false
		case *syntax.Subshell:
			unsafeReason = "subshell"
			return false
		case *syntax.FuncDecl:
			unsafeReason = "function declaration"
			return false
		case *syntax.Redirect:
			if !redirectIsReadOnly(n) {
				unsafeReason = "write redirect"
				return false
			}
		case *syntax.CallExpr:
			callExprs = append(callExprs, n)
		}
		return true
	})
	if unsafeReason != "" {
		return false, unsafeReason
	}

	for _, ce := range callExprs {
		// CallExpr with only assignments (FOO=bar) and no command:
		// safe in isolation (no execution). Allow.
		if len(ce.Args) == 0 {
			continue
		}
		rawName, ok := staticWordValue(ce.Args[0])
		if !ok {
			return false, "dynamic command name"
		}
		name := filepath.Base(rawName)
		if !readOnlyBashCommands[name] {
			return false, "command not in read-only allowlist"
		}
		// Per-command flag checks — same allowlisted binaries can be
		// used in mutating ways (sed -i, find -exec, tail -F + spawn).
		if !commandFlagsReadOnly(name, ce.Args[1:]) {
			return false, name + " used with mutating flag"
		}
	}
	return true, ""
}

// redirectIsReadOnly returns true for redirections that don't write
// to a new file. Read-from-file (<), here-docs (<<, <<-), here-strings
// (<<<), and fd-duplication (2>&1, 2>&-) are always allowed.
// Output-to-file redirects (>, >>) are allowed only when the target
// is literally /dev/null (common pattern for silencing stderr); any
// other file target is rejected.
func redirectIsReadOnly(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.DplIn:
		return true
	case syntax.DplOut:
		// 2>&1, 2>&-, etc. Duplicate or close an fd; no file write.
		return true
	case syntax.RdrOut, syntax.AppOut:
		// > /dev/null and >> /dev/null are harmless silencing.
		if r.Word != nil {
			if val, ok := staticWordValue(r.Word); ok && val == "/dev/null" {
				return true
			}
		}
	}
	return false
}

// readOnlyBashCommands is the binary-name allowlist. A command's
// `filepath.Base(arg[0])` must match one of these for the classifier
// to allow it. The list is intentionally conservative — start small
// and grow as real false-negatives surface.
//
// Excluded on purpose:
//   - awk: `awk 'BEGIN{system("rm")}'` is a side-effect escape.
//   - tee: writes its input to a file.
//   - env, xargs: prefix commands that run other commands; the
//     wrapped command is what matters, and we don't recursively
//     classify their args here.
//   - git, hg: subcommand-dependent; need a separate classifier.
//   - bash, sh, zsh, python, node, ruby, perl: arbitrary execution.
//   - source, ., eval, exec: shell metaprogramming.
var readOnlyBashCommands = map[string]bool{
	// Filesystem inspection.
	"pwd":      true,
	"ls":       true,
	"find":     true,
	"stat":     true,
	"file":     true,
	"du":       true,
	"df":       true,
	"wc":       true,
	"readlink": true,
	"realpath": true,
	"dirname":  true,
	"basename": true,
	"tree":     true,
	// File reading.
	"cat":     true,
	"head":    true,
	"tail":    true,
	"hexdump": true,
	"xxd":     true,
	"od":      true,
	"less":    true,
	"more":    true,
	// Text processing (read-only).
	"grep":  true,
	"egrep": true,
	"fgrep": true,
	"rg":    true,
	"ag":    true,
	"cut":   true,
	"sort":  true,
	"uniq":  true,
	"tr":    true,
	"sed":   true, // -i refused below
	"paste": true,
	"col":   true,
	// Simple output / formatting.
	"echo":   true,
	"printf": true,
	"yes":    true,
	// System info.
	"date":     true,
	"hostname": true,
	"uname":    true,
	"id":       true,
	"groups":   true,
	"whoami":   true,
	"which":    true,
	"type":     true,
	"command":  true,
	// Read-only conditional builtins.
	"test":  true,
	"[":     true,
	"true":  true,
	"false": true,
	"":      true, // empty fragments from heredoc-only stmts
}

// commandFlagsReadOnly checks per-command flag patterns that flip
// an otherwise-safe binary into a mutating one. Returns false when
// the command is being used to write/modify.
func commandFlagsReadOnly(name string, args []*syntax.Word) bool {
	switch name {
	case "sed":
		// -i / --in-place edits files in place.
		for _, a := range args {
			val, ok := staticWordValue(a)
			if !ok {
				return false
			}
			if val == "-i" || val == "--in-place" || strings.HasPrefix(val, "-i") || strings.HasPrefix(val, "--in-place=") {
				return false
			}
		}
	case "find":
		// -exec / -execdir / -delete / -fprint* run commands or
		// mutate the filesystem.
		for _, a := range args {
			val, ok := staticWordValue(a)
			if !ok {
				return false
			}
			switch val {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete":
				return false
			}
			if strings.HasPrefix(val, "-fprint") {
				return false
			}
		}
	case "tail":
		// -F respawns when a file rotates; an attacker who can
		// rotate files could redirect output. -f is fine.
		for _, a := range args {
			val, ok := staticWordValue(a)
			if !ok {
				return false
			}
			if val == "-F" || strings.HasPrefix(val, "--follow=name") {
				return false
			}
		}
	}
	return true
}
