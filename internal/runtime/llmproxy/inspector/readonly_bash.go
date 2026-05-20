package inspector

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// IsReadOnlyBashCommand reports whether cmd is composed entirely of
// side-effect-free shell commands with no write redirects, substitutions, or
// unknown binaries. It is intentionally conservative: unknown commands require
// normal task scope or review.
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
			if !readonlyRedirect(n) {
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
		if len(ce.Assigns) > 0 {
			return false, "environment assignment"
		}
		if len(ce.Args) == 0 {
			continue
		}
		rawName, ok := staticWordValue(ce.Args[0])
		if !ok {
			return false, "dynamic command name"
		}
		if strings.Contains(rawName, "/") {
			return false, "qualified command path"
		}
		name := rawName
		if !readOnlyBashCommands[name] {
			return false, "command not in read-only allowlist"
		}
		if !commandFlagsReadOnly(name, ce.Args[1:]) {
			return false, name + " used with mutating flag"
		}
	}
	return true, ""
}

func readonlyRedirect(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.DplIn:
		return true
	case syntax.RdrOut, syntax.AppOut:
		if r.Word != nil {
			if val, ok := staticWordValue(r.Word); ok && val == "/dev/null" {
				return true
			}
		}
	}
	return false
}

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
	// File reading.
	"cat":     true,
	"head":    true,
	"tail":    true,
	"hexdump": true,
	"od":      true,
	// Text processing.
	"grep":  true,
	"egrep": true,
	"fgrep": true,
	"rg":    true,
	"cut":   true,
	"sort":  true,
	"uniq":  true,
	"tr":    true,
	"paste": true,
	"col":   true,
	// Simple output / formatting.
	"echo":   true,
	"printf": true,
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
	// Read-only conditionals.
	"test":  true,
	"[":     true,
	"true":  true,
	"false": true,
	"":      true,
}

func commandFlagsReadOnly(name string, args []*syntax.Word) bool {
	values, ok := staticWordValues(args)
	if !ok {
		return false
	}
	switch name {
	case "date":
		return dateArgsReadOnly(values)
	case "file":
		for _, val := range values {
			if longFlag(val, "compile") || shortOptionHas(val, 'C') {
				return false
			}
		}
	case "find":
		for _, val := range values {
			switch val {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete":
				return false
			}
			if strings.HasPrefix(val, "-fprint") || strings.HasPrefix(val, "-fls") || strings.HasPrefix(val, "-fprintf") {
				return false
			}
		}
	case "hostname":
		for _, val := range values {
			if val == "--" {
				continue
			}
			if !strings.HasPrefix(val, "-") {
				return false
			}
		}
	case "rg":
		for _, val := range values {
			if longFlag(val, "pre") || longFlag(val, "hostname-bin") {
				return false
			}
		}
	case "sort":
		for _, val := range values {
			if longFlag(val, "output") || shortOptionHas(val, 'o') {
				return false
			}
		}
	case "tail":
		for _, val := range values {
			if longFlag(val, "follow") || shortOptionHas(val, 'f') || shortOptionHas(val, 'F') {
				return false
			}
		}
	case "uniq":
		return uniqArgsReadOnly(values)
	case "command":
		sawInspect := false
		for _, val := range values {
			if val == "-v" || val == "-V" {
				sawInspect = true
				continue
			}
			if strings.HasPrefix(val, "-") {
				return false
			}
			if !sawInspect {
				return false
			}
		}
		if !sawInspect {
			return false
		}
	}
	return true
}

func staticWordValues(args []*syntax.Word) ([]string, bool) {
	values := make([]string, 0, len(args))
	for _, a := range args {
		val, ok := staticWordValue(a)
		if !ok {
			return nil, false
		}
		values = append(values, val)
	}
	return values, true
}

func longFlag(val, name string) bool {
	return val == "--"+name || strings.HasPrefix(val, "--"+name+"=")
}

func shortOptionHas(val string, want rune) bool {
	if len(val) < 2 || val[0] != '-' || val == "--" || strings.HasPrefix(val, "--") {
		return false
	}
	for _, r := range val[1:] {
		if r == want {
			return true
		}
	}
	return false
}

func dateArgsReadOnly(values []string) bool {
	for i := 0; i < len(values); i++ {
		val := values[i]
		switch {
		case val == "--":
			for _, rest := range values[i+1:] {
				if !strings.HasPrefix(rest, "+") {
					return false
				}
			}
			return true
		case strings.HasPrefix(val, "+"):
			continue
		case val == "-r" || val == "-z":
			i++
			if i >= len(values) {
				return false
			}
		case strings.HasPrefix(val, "-r") || strings.HasPrefix(val, "-z"):
			continue
		case val == "-f":
			// BSD date uses -j -f for parsing without setting the clock, but
			// the cross-platform surface is too subtle for taskless shell.
			return false
		case val == "-I" || strings.HasPrefix(val, "-I"):
			continue
		case val == "-j" || val == "-n" || val == "-R" || val == "-u":
			continue
		case strings.HasPrefix(val, "-v"):
			continue
		case val == "-s" || strings.HasPrefix(val, "-s") || longFlag(val, "set"):
			return false
		case strings.HasPrefix(val, "-"):
			return false
		default:
			return false
		}
	}
	return true
}

func uniqArgsReadOnly(values []string) bool {
	operands := 0
	for i := 0; i < len(values); i++ {
		val := values[i]
		switch {
		case val == "--":
			operands += len(values) - i - 1
			return operands <= 1
		case val == "-f" || val == "-s" || val == "-w":
			i++
			if i >= len(values) {
				return false
			}
		case strings.HasPrefix(val, "-f") || strings.HasPrefix(val, "-s") || strings.HasPrefix(val, "-w"):
			continue
		case strings.HasPrefix(val, "--skip-fields=") ||
			strings.HasPrefix(val, "--skip-chars=") ||
			strings.HasPrefix(val, "--check-chars=") ||
			strings.HasPrefix(val, "--group=") ||
			strings.HasPrefix(val, "--all-repeated="):
			continue
		case val == "--skip-fields" || val == "--skip-chars" || val == "--check-chars" || val == "--group" || val == "--all-repeated":
			i++
			if i >= len(values) {
				return false
			}
		case strings.HasPrefix(val, "-"):
			continue
		default:
			operands++
			if operands > 1 {
				return false
			}
		}
	}
	return true
}
