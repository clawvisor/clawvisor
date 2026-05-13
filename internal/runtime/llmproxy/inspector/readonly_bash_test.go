package inspector

import "testing"

func TestIsReadOnlyBashCommand_AcceptsCommonReads(t *testing.T) {
	cases := []string{
		"pwd",
		"ls",
		"ls -la",
		"ls -la /tmp/landing-b",
		"cat /etc/hosts",
		"head -n 20 /var/log/system.log",
		"tail -n 50 /var/log/system.log",
		"find /tmp -name '*.json'",
		"grep -r 'foo' src/",
		"rg pattern .",
		"wc -l README.md",
		"stat -c %s /tmp/file",
		"du -sh /tmp",
		"df -h",
		"whoami",
		"id",
		"echo hello",
		"printf '%s\\n' done",
		"date",
		"hostname",
		// Pipelines of reads.
		"ls /tmp | grep landing",
		"cat README.md | head -n 5",
		"find . -name '*.go' | wc -l",
		"ls -la | grep '^d' | wc -l",
		// Chain of reads — && and || are single statements with a
		// BinaryCmd Cmd type. Both branches are still classified.
		"pwd && ls -la",
		"ls foo || echo missing",
		// Stderr redirect to /dev/null is fine (no file write).
		"ls /missing 2>/dev/null",
		// fd-duplication is harmless.
		"ls /missing 2>&1",
		// Heredoc (input redirect) is fine.
		"cat <<EOF\nhello\nEOF\n",
		// Read redirect.
		"wc -l < /tmp/file",
		// sed without -i.
		"sed -n '1,10p' README.md",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if !ok {
				t.Errorf("expected read-only, got refusal: reason=%q\n  cmd=%s", reason, cmd)
			}
		})
	}
}

func TestIsReadOnlyBashCommand_RejectsMutations(t *testing.T) {
	cases := map[string]string{
		"rm -rf /tmp/x":                              "command not in read-only allowlist",
		"mv a b":                                     "command not in read-only allowlist",
		"cp a b":                                     "command not in read-only allowlist",
		"mkdir /tmp/x":                               "command not in read-only allowlist",
		"touch /tmp/file":                            "command not in read-only allowlist",
		"chmod +x /tmp/file":                         "command not in read-only allowlist",
		"curl https://example.com":                   "command not in read-only allowlist",
		"wget https://example.com/file":              "command not in read-only allowlist",
		"git commit -m 'wip'":                        "command not in read-only allowlist",
		"npm install":                                "command not in read-only allowlist",
		"bash script.sh":                             "command not in read-only allowlist",
		"python -c 'print(1)'":                       "command not in read-only allowlist",
		"sed -i 's/x/y/' file":                       "sed used with mutating flag",
		"find . -name '*.tmp' -delete":               "find used with mutating flag",
		"find . -name '*.go' -exec ls {} \\;":        "find used with mutating flag",
		// Write redirects.
		"ls > /tmp/out":                              "write redirect",
		"echo hi >> /tmp/log":                        "write redirect",
		"cat /etc/hosts > /tmp/copy":                 "write redirect",
		// Multi-statement is conservatively refused — model should use && instead.
		"pwd; ls":                                    "multiple statements",
		// Command substitution — could embed a write.
		"echo $(rm -rf /tmp)":                        "command substitution",
		// Subshell — could contain anything.
		"(cd /tmp && rm -rf .)":                      "subshell",
		// Heredoc to a write destination is caught by the write redirect.
		"cat <<EOF > /tmp/x\nhello\nEOF\n":           "write redirect",
	}
	for cmd, wantReason := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Fatalf("expected refusal for %q, got allowed", cmd)
			}
			if reason == "" {
				t.Errorf("refusal must include a reason; cmd=%q", cmd)
			}
			// We don't assert exact reason text for every case
			// (the classifier evolves) — just spot-check the
			// rejection. The map's wantReason is documentation
			// for the intent.
			_ = wantReason
		})
	}
}

// Conservative: pipelines that contain ANY refused command must be
// refused as a whole. The classifier doesn't try to be clever about
// "stdout doesn't flow through" — if `rm` appears anywhere in a
// pipeline, the whole thing is unsafe.
func TestIsReadOnlyBashCommand_RejectsMixedPipelines(t *testing.T) {
	cases := []string{
		"ls | rm",
		"cat /etc/hosts && rm -rf /tmp",
		"ls > /tmp/out && pwd",
		"pwd || curl https://evil.example",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, _ := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Errorf("mixed pipeline must be refused: %q", cmd)
			}
		})
	}
}

// P0 regression: the `command` shell builtin without -v/-V runs its
// argument. Allowlisting `command` as read-only was a security bypass
// — `command rm -rf /` would have passed. Only the inspection flags
// (-v / -V print what a name resolves to) are safe.
func TestIsReadOnlyBashCommand_CommandBuiltinBypass(t *testing.T) {
	allowed := []string{
		"command -v rm",
		"command -V cat",
		"command -v which",
	}
	for _, cmd := range allowed {
		t.Run("allow_"+cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if !ok {
				t.Errorf("%q should be read-only (inspect flag), got refusal: %s", cmd, reason)
			}
		})
	}
	refused := []string{
		"command rm -rf /tmp",
		"command ls",
		"command -p rm /tmp/x",
		"command",
	}
	for _, cmd := range refused {
		t.Run("refuse_"+cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Fatalf("SECURITY: `command` without -v/-V must refuse — %q passed", cmd)
			}
			if reason == "" {
				t.Errorf("refusal must include a reason")
			}
		})
	}
}

// Empty / malformed inputs must refuse cleanly, never crash.
func TestIsReadOnlyBashCommand_EdgeCases(t *testing.T) {
	cases := []string{"", "   ", "$(", `'unterminated`}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Errorf("edge case %q should refuse", cmd)
			}
			if reason == "" {
				t.Errorf("edge case %q should have a non-empty reason", cmd)
			}
		})
	}
}

// Sanity: dynamic command names (variable interpolation) must refuse
// — we can't know what they'll resolve to at runtime.
func TestIsReadOnlyBashCommand_DynamicCommandNameRefused(t *testing.T) {
	cases := []string{
		`$CMD foo`,
		`"$BIN" -la`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, _ := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Errorf("dynamic command must refuse: %q", cmd)
			}
		})
	}
}
