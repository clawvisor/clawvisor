package inspector

import "testing"

func TestCommandReferencesSensitivePathFlagsCommonSecrets(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"cat home ssh key", "cat ~/.ssh/id_rsa"},
		{"cat absolute ssh key", "cat /Users/eric/.ssh/id_ed25519"},
		{"cat home expansion ssh key", "cat $HOME/.ssh/id_rsa"},
		{"cat braced home expansion ssh key", "cat ${HOME}/.ssh/id_ed25519"},
		{"head .env", "head -n 5 .env"},
		{"rg option value env file", "rg --ignore-file=.env SECRET ."},
		{"grep over aws dir", "grep -R access_key ~/.aws"},
		{"cat pem", "cat certs/tls.pem"},
		{"cat netrc", "cat ~/.netrc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, ok := CommandReferencesSensitivePath(tc.cmd)
			if !ok {
				t.Fatalf("expected %q to reference a sensitive path", tc.cmd)
			}
			if reason == "" {
				t.Fatalf("expected non-empty reason for %q", tc.cmd)
			}
		})
	}
}

func TestCommandReferencesSensitivePathIgnoresBenignCommands(t *testing.T) {
	cases := []string{
		"cat README.md",
		"ls -la /tmp",
		"grep TODO src/",
		"find . -name '*.go'",
		"cat .env.example", // committed template, not sensitive
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if _, _, ok := CommandReferencesSensitivePath(cmd); ok {
				t.Fatalf("expected %q to be benign", cmd)
			}
		})
	}
}

func TestIsReadOnlyBashCommandAcceptsCommonReads(t *testing.T) {
	cases := []string{
		"pwd",
		"ls -la /tmp",
		"cat /etc/hosts",
		"head -n 20 README.md",
		"tail -n 5 README.md",
		"find . -name '*.go'",
		"rg pattern .",
		"ls /tmp | grep landing | wc -l",
		"pwd && ls -la",
		"ls foo || echo missing",
		"ls /missing 2>/dev/null",
		"wc -l < /tmp/file",
		"command -v rg",
		"date +%s",
		"date -u +%FT%TZ",
		"hostname",
		"hostname --fqdn",
		"hostname -I",
		"cat <&0",
		"printf '%s\\n' hello",
		"printf -- -v",
		"sort README.md",
		"uniq README.md",
		"head -40 README.md",
		"tail -40 README.md",
		"ls /tmp | head -40",
		"ls /tmp | tail -5",
		"echo ---",
		"echo -x",
		"echo -- foo",
		"echo --foo",
		"echo --bar=baz",
		"echo -en hi",
		"echo -e hi",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if !ok {
				t.Fatalf("expected read-only, got reason=%q", reason)
			}
		})
	}
}

func TestIsReadOnlyBashCommandRejectsMutationsAndEscapes(t *testing.T) {
	cases := []string{
		"rm -rf /tmp/x",
		"mkdir /tmp/new",
		"curl https://example.com",
		"python -c 'print(1)'",
		"sed -i 's/x/y/' file",
		"find . -name '*.tmp' -delete",
		"find . -name '*.go' -exec ls {} \\;",
		"ls > /tmp/out",
		"cat < /dev/tcp/example.com/80",
		"cat < /dev/udp/example.com/53",
		`cat < "$INPUT_PATH"`,
		"cat <&/tmp/fd",
		"cat <> /tmp/new-file",
		"cat 1>&/tmp/out",
		"./ls -la",
		"/tmp/grep pattern file",
		"sed -n '1,10p' README.md",
		"sed -n '1w/tmp/out' file",
		"sed '1e touch /tmp/x' file",
		"less README.md",
		"more README.md",
		"xxd README.md",
		"tree /tmp",
		"ag pattern .",
		"yes ok",
		"file -C -m /tmp/magic",
		"find . -name '*.go' -fls /tmp/out",
		"find . -name '*.go' -fprintf /tmp/out '%p\\n'",
		"rg --pre /tmp/filter pattern .",
		"rg --hostname-bin /tmp/hostname pattern .",
		"sort -o /tmp/out README.md",
		"sort --output=/tmp/out README.md",
		"sort --compress-program=/tmp/pwn README.md",
		"sort --compress-program /tmp/pwn README.md",
		"sort --definitely-not-a-safe-flag README.md",
		"uniq README.md /tmp/out",
		"uniq --definitely-not-a-safe-flag README.md",
		"date 01020304",
		"date -s tomorrow",
		"date --set=tomorrow",
		"hostname new-name",
		"hostname --file=/tmp/name",
		"hostname -F/tmp/name",
		"hostname -F /tmp/name",
		"tail -f /tmp/log",
		"tail --follow=name /tmp/log",
		"printf -v PATH /tmp && ls",
		"printf -vPATH /tmp && ls",
		"PATH=/tmp ls",
		"pwd; ls",
		"echo $(rm -rf /tmp/x)",
		"(cd /tmp && rm -rf .)",
		`$CMD foo`,
		`"$(which ls)" -la`,
		"command rm -rf /tmp",
		"cat -40 README.md",
		"head -40x README.md",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			ok, reason := IsReadOnlyBashCommand(cmd)
			if ok {
				t.Fatalf("expected refusal for %q", cmd)
			}
			if reason == "" {
				t.Fatalf("expected refusal reason")
			}
		})
	}
}
