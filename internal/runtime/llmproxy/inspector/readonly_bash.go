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
	case syntax.RdrIn:
		if r.Word == nil {
			return false
		}
		val, ok := staticWordValue(r.Word)
		return ok && !isBashNetworkPseudoPath(val)
	case syntax.DplIn:
		if r.Word == nil {
			return false
		}
		val, ok := staticWordValue(r.Word)
		return ok && (val == "-" || allDigits(val))
	case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
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

func isBashNetworkPseudoPath(path string) bool {
	return strings.HasPrefix(path, "/dev/tcp/") || strings.HasPrefix(path, "/dev/udp/")
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
	case "find":
		return findArgsReadOnly(values)
	case "hostname":
		return hostnameArgsReadOnly(values)
	case "printf":
		return printfArgsReadOnly(values)
	case "uniq":
		return uniqArgsReadOnly(values)
	case "command":
		return commandArgsReadOnly(values)
	case "test":
		return true
	case "[":
		return len(values) > 0 && values[len(values)-1] == "]"
	}
	spec, ok := readOnlyCommandOptions[name]
	return ok && argsMatchOptionSpec(values, spec)
}

type optionSpec struct {
	shortNoArg string
	shortArg   string
	longNoArg  map[string]bool
	longArg    map[string]bool
	operands   operandMode
}

type operandMode int

const (
	operandsNone operandMode = iota
	operandsAny
	operandsAtMostOne
)

var readOnlyCommandOptions = map[string]optionSpec{
	"":         {operands: operandsNone},
	"true":     {operands: operandsNone},
	"false":    {operands: operandsNone},
	"whoami":   noOperandSpec("help", "version"),
	"groups":   anyOperandSpec("help", "version"),
	"id":       {shortNoArg: "GgnruZ", longNoArg: flagSet("group", "groups", "name", "real", "user", "zero", "context", "help", "version"), operands: operandsAny},
	"pwd":      {shortNoArg: "LP", longNoArg: flagSet("logical", "physical", "help", "version"), operands: operandsNone},
	"uname":    {shortNoArg: "asnrvmpio", longNoArg: flagSet("all", "kernel-name", "nodename", "kernel-release", "kernel-version", "machine", "processor", "hardware-platform", "operating-system", "help", "version"), operands: operandsNone},
	"which":    {shortNoArg: "a", longNoArg: flagSet("all", "skip-dot", "skip-tilde", "show-dot", "show-tilde", "tty-only", "help", "version"), operands: operandsAny},
	"type":     {shortNoArg: "afptP", operands: operandsAny},
	"ls":       {shortNoArg: "abcdfghiklmnopqrstuvwxyABCDFGHILNQRSTUXZ1", shortArg: "wT", longNoArg: flagSet("all", "almost-all", "author", "escape", "directory", "dired", "classify", "file-type", "full-time", "group-directories-first", "human-readable", "inode", "dereference", "numeric-uid-gid", "literal", "hide-control-chars", "show-control-chars", "quote-name", "recursive", "reverse", "size", "width", "context", "help", "version"), longArg: flagSet("block-size", "color", "colour", "format", "hide", "ignore", "indicator-style", "quoting-style", "sort", "tabsize", "time", "time-style"), operands: operandsAny},
	"stat":     {shortNoArg: "Lct", shortArg: "cf", longNoArg: flagSet("dereference", "terse", "cached", "help", "version"), longArg: flagSet("format", "printf", "file-system"), operands: operandsAny},
	"file":     {shortNoArg: "bcdEhikLlnNprsSvzZ0", shortArg: "eFfmP", longNoArg: flagSet("brief", "checking-printout", "exclude-quiet", "extension", "keep-going", "list", "dereference", "no-dereference", "no-buffer", "no-pad", "preserve-date", "raw", "special-files", "apple", "mime", "mime-type", "mime-encoding", "uncompress", "uncompress-noreport", "print0", "help", "version"), longArg: flagSet("exclude", "separator", "files-from", "magic-file", "parameter"), operands: operandsAny},
	"du":       {shortNoArg: "0abcDhHklLmsSx", shortArg: "Btd", longNoArg: flagSet("null", "all", "apparent-size", "bytes", "total", "dereference-args", "human-readable", "si", "count-links", "dereference", "separate-dirs", "summarize", "one-file-system", "help", "version"), longArg: flagSet("block-size", "max-depth", "threshold", "time", "time-style", "exclude", "exclude-from", "files0-from"), operands: operandsAny},
	"df":       {shortNoArg: "ahiHklPTv", shortArg: "Btx", longArg: flagSet("block-size", "output", "total", "type", "exclude-type"), longNoArg: flagSet("all", "human-readable", "si", "inodes", "local", "no-sync", "portability", "print-type", "sync", "help", "version"), operands: operandsAny},
	"wc":       {shortNoArg: "clLmw", longNoArg: flagSet("bytes", "chars", "lines", "max-line-length", "words", "help", "version"), operands: operandsAny},
	"readlink": {shortNoArg: "efmnqsvz", longNoArg: flagSet("canonicalize", "canonicalize-existing", "canonicalize-missing", "no-newline", "quiet", "silent", "verbose", "zero", "help", "version"), operands: operandsAny},
	"realpath": {shortNoArg: "eLmqsz", shortArg: "P", longNoArg: flagSet("canonicalize-existing", "logical", "canonicalize-missing", "no-symlinks", "quiet", "strip", "zero", "help", "version"), longArg: flagSet("relative-to", "relative-base"), operands: operandsAny},
	"dirname":  {shortNoArg: "z", longNoArg: flagSet("zero", "help", "version"), operands: operandsAny},
	"basename": {shortNoArg: "azs", longArg: flagSet("suffix"), longNoArg: flagSet("multiple", "zero", "help", "version"), operands: operandsAny},
	"cat":      {shortNoArg: "AbeEnstTuv", longNoArg: flagSet("show-all", "number-nonblank", "show-ends", "number", "squeeze-blank", "show-tabs", "show-nonprinting", "help", "version"), operands: operandsAny},
	"head":     {shortNoArg: "cqvz", shortArg: "cn", longNoArg: flagSet("quiet", "silent", "verbose", "zero-terminated", "help", "version"), longArg: flagSet("bytes", "lines"), operands: operandsAny},
	"tail":     {shortNoArg: "qvz", shortArg: "cn", longNoArg: flagSet("quiet", "silent", "verbose", "zero-terminated", "help", "version"), longArg: flagSet("bytes", "lines", "pid", "sleep-interval", "max-unchanged-stats"), operands: operandsAny},
	"hexdump":  {shortNoArg: "bcdCvx", shortArg: "efnos", longNoArg: flagSet("canonical", "help", "version"), longArg: flagSet("format", "format-file", "length", "skip"), operands: operandsAny},
	"od":       {shortNoArg: "AbcDdfFHiIloOsvx", shortArg: "jNtw", longNoArg: flagSet("address-radix", "skip-bytes", "read-bytes", "format", "output-duplicates", "strings", "traditional", "width", "help", "version"), longArg: flagSet("endian"), operands: operandsAny},
	"grep":     {shortNoArg: "EFGPiwxvcsnHhLloqbrRIUVzZ", shortArg: "ABCDefm", longNoArg: flagSet("basic-regexp", "extended-regexp", "fixed-strings", "perl-regexp", "ignore-case", "word-regexp", "line-regexp", "invert-match", "count", "line-number", "with-filename", "no-filename", "files-with-matches", "files-without-match", "only-matching", "quiet", "silent", "byte-offset", "text", "binary", "recursive", "dereference-recursive", "initial-tab", "unix-byte-offsets", "null", "null-data", "help", "version"), longArg: flagSet("after-context", "before-context", "binary-files", "color", "colour", "context", "devices", "directories", "exclude", "exclude-dir", "exclude-from", "group-separator", "include", "label", "max-count"), operands: operandsAny},
	"egrep":    {shortNoArg: "EFGPiwxvcsnHhLloqbrRIUVzZ", shortArg: "ABCDefm", longNoArg: flagSet("basic-regexp", "extended-regexp", "fixed-strings", "perl-regexp", "ignore-case", "word-regexp", "line-regexp", "invert-match", "count", "line-number", "with-filename", "no-filename", "files-with-matches", "files-without-match", "only-matching", "quiet", "silent", "byte-offset", "text", "binary", "recursive", "dereference-recursive", "initial-tab", "unix-byte-offsets", "null", "null-data", "help", "version"), longArg: flagSet("after-context", "before-context", "binary-files", "color", "colour", "context", "devices", "directories", "exclude", "exclude-dir", "exclude-from", "group-separator", "include", "label", "max-count"), operands: operandsAny},
	"fgrep":    {shortNoArg: "EFGPiwxvcsnHhLloqbrRIUVzZ", shortArg: "ABCDefm", longNoArg: flagSet("basic-regexp", "extended-regexp", "fixed-strings", "perl-regexp", "ignore-case", "word-regexp", "line-regexp", "invert-match", "count", "line-number", "with-filename", "no-filename", "files-with-matches", "files-without-match", "only-matching", "quiet", "silent", "byte-offset", "text", "binary", "recursive", "dereference-recursive", "initial-tab", "unix-byte-offsets", "null", "null-data", "help", "version"), longArg: flagSet("after-context", "before-context", "binary-files", "color", "colour", "context", "devices", "directories", "exclude", "exclude-dir", "exclude-from", "group-separator", "include", "label", "max-count"), operands: operandsAny},
	"rg":       {shortNoArg: "0FHIJSUVchilnuvwx", shortArg: "ABCegjtm", longNoArg: flagSet("after-context", "before-context", "block-buffered", "byte-offset", "case-sensitive", "color", "column", "context", "count", "count-matches", "crlf", "debug", "dfa-size-limit", "files", "files-with-matches", "files-without-match", "fixed-strings", "follow", "glob", "heading", "hidden", "ignore-case", "json", "line-buffered", "line-number", "line-regexp", "max-columns-preview", "no-heading", "no-ignore", "no-ignore-dot", "no-ignore-exclude", "no-ignore-files", "no-ignore-global", "no-ignore-messages", "no-ignore-parent", "no-line-number", "no-messages", "no-require-git", "no-unicode", "null", "one-file-system", "passthru", "pcre2", "pretty", "quiet", "smart-case", "stats", "text", "trim", "type-list", "unrestricted", "vimgrep", "with-filename", "word-regexp", "help", "version"), longArg: flagSet("after-context", "before-context", "binary", "color", "colors", "context", "context-separator", "dfa-size-limit", "encoding", "engine", "field-context-separator", "field-match-separator", "glob", "glob-case-insensitive", "iglob", "ignore-file", "ignore-file-case-insensitive", "json-path", "max-columns", "max-count", "max-depth", "max-filesize", "mmap", "multiline", "multiline-dotall", "path-separator", "regex-size-limit", "regexp", "replace", "sort", "sortr", "threads", "type", "type-add", "type-clear", "type-not"), operands: operandsAny},
	"cut":      {shortNoArg: "nsz", shortArg: "bcdf", longNoArg: flagSet("complement", "only-delimited", "zero-terminated", "help", "version"), longArg: flagSet("bytes", "characters", "delimiter", "fields", "output-delimiter"), operands: operandsAny},
	"sort":     {shortNoArg: "bcdfghiMmnRruVz", shortArg: "kSstT", longNoArg: flagSet("check", "debug", "dictionary-order", "general-numeric-sort", "human-numeric-sort", "ignore-case", "ignore-leading-blanks", "ignore-nonprinting", "month-sort", "numeric-sort", "random-sort", "reverse", "unique", "version-sort", "zero-terminated", "help", "version"), longArg: flagSet("batch-size", "buffer-size", "field-separator", "key", "parallel", "random-source", "sort", "temporary-directory"), operands: operandsAny},
	"tr":       {shortNoArg: "cdst", longNoArg: flagSet("complement", "delete", "squeeze-repeats", "truncate-set1", "help", "version"), operands: operandsAny},
	"paste":    {shortNoArg: "sz", shortArg: "d", longNoArg: flagSet("serial", "zero-terminated", "help", "version"), longArg: flagSet("delimiters"), operands: operandsAny},
	"col":      {shortNoArg: "bfhpx", operands: operandsAny},
	"echo":     {shortNoArg: "enE", longNoArg: flagSet("help", "version"), operands: operandsAny},
}

func flagSet(names ...string) map[string]bool {
	flags := make(map[string]bool, len(names))
	for _, name := range names {
		flags[name] = true
	}
	return flags
}

func noOperandSpec(longNoArg ...string) optionSpec {
	return optionSpec{longNoArg: flagSet(longNoArg...), operands: operandsNone}
}

func anyOperandSpec(longNoArg ...string) optionSpec {
	return optionSpec{longNoArg: flagSet(longNoArg...), operands: operandsAny}
}

func argsMatchOptionSpec(values []string, spec optionSpec) bool {
	operands := 0
	for i := 0; i < len(values); i++ {
		val := values[i]
		switch {
		case val == "--":
			operands += len(values) - i - 1
			return operandCountAllowed(operands, spec.operands)
		case val == "-" || !strings.HasPrefix(val, "-"):
			operands++
			if !operandCountAllowed(operands, spec.operands) {
				return false
			}
		case strings.HasPrefix(val, "--"):
			name, hasValue := splitLongOption(val)
			if hasValue {
				if !spec.longArg[name] {
					return false
				}
				continue
			}
			if spec.longNoArg[name] {
				continue
			}
			if spec.longArg[name] {
				i++
				if i >= len(values) {
					return false
				}
				continue
			}
			return false
		default:
			if !shortOptionsMatch(val, values, &i, spec) {
				return false
			}
		}
	}
	return true
}

func splitLongOption(value string) (string, bool) {
	name, _, hasArg := strings.Cut(strings.TrimPrefix(value, "--"), "=")
	return name, hasArg
}

func shortOptionsMatch(value string, values []string, index *int, spec optionSpec) bool {
	options := []rune(value[1:])
	for i, opt := range options {
		if strings.ContainsRune(spec.shortNoArg, opt) {
			continue
		}
		if strings.ContainsRune(spec.shortArg, opt) {
			if i == len(options)-1 {
				*index = *index + 1
				if *index >= len(values) {
					return false
				}
			}
			return true
		}
		return false
	}
	return len(options) > 0
}

func operandCountAllowed(count int, mode operandMode) bool {
	switch mode {
	case operandsNone:
		return count == 0
	case operandsAtMostOne:
		return count <= 1
	case operandsAny:
		return true
	default:
		return false
	}
}

func findArgsReadOnly(values []string) bool {
	for i := 0; i < len(values); i++ {
		val := values[i]
		switch val {
		case "--", "--help", "--version",
			"!", "(", ")", ",", "-a", "-and", "-o", "-or", "-not",
			"-H", "-L", "-P", "-depth", "-xdev", "-mount", "-noleaf",
			"-daystart", "-ignore_readdir_race", "-noignore_readdir_race",
			"-print", "-print0", "-printf", "-ls", "-quit",
			"-empty", "-readable", "-writable", "-executable", "-true", "-false":
			if val == "-printf" {
				i++
				if i >= len(values) {
					return false
				}
			}
			continue
		case "-D", "-files0-from",
			"-name", "-iname", "-path", "-ipath", "-regex", "-iregex", "-regextype",
			"-type", "-xtype", "-maxdepth", "-mindepth", "-size",
			"-mtime", "-mmin", "-ctime", "-cmin", "-atime", "-amin",
			"-user", "-group", "-uid", "-gid", "-perm", "-inum", "-links",
			"-fstype", "-samefile", "-newer", "-used":
			i++
			if i >= len(values) {
				return false
			}
			continue
		}
		if strings.HasPrefix(val, "-O") && len(val) > 2 && allDigits(val[2:]) {
			continue
		}
		if strings.HasPrefix(val, "-newer") && val != "-newer" {
			i++
			if i >= len(values) {
				return false
			}
			continue
		}
		if strings.HasPrefix(val, "-") {
			return false
		}
	}
	return true
}

func printfArgsReadOnly(values []string) bool {
	for _, val := range values {
		if val == "--" {
			return true
		}
		if val == "-v" || strings.HasPrefix(val, "-v") {
			return false
		}
	}
	return true
}

func hostnameArgsReadOnly(values []string) bool {
	for _, val := range values {
		switch val {
		case "-s", "--short", "-a", "--alias", "-i", "--ip-address", "-I", "--all-ip-addresses",
			"-d", "--domain", "-f", "--fqdn", "--long", "-A", "--all-fqdns", "-y", "--yp", "--nis":
			continue
		default:
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
	return argsMatchOptionSpec(values, optionSpec{
		shortNoArg: "cduizD",
		shortArg:   "fsw",
		longNoArg:  flagSet("count", "repeated", "unique", "ignore-case", "zero-terminated", "help", "version"),
		longArg:    flagSet("skip-fields", "skip-chars", "check-chars", "group", "all-repeated"),
		operands:   operandsAtMostOne,
	})
}

func commandArgsReadOnly(values []string) bool {
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
	return sawInspect
}
