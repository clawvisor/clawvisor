package sensitivepaths

import "testing"

func TestIsSensitivePathClassifiesKnownSecrets(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"dotenv at workspace root", "/repo/app/.env"},
		{"dotenv variant", "/repo/app/.env.production"},
		{"netrc in home", "/Users/eric/.netrc"},
		{"npmrc anywhere", "/repo/app/.npmrc"},
		{"ssh private key by name", "/Users/eric/.ssh/id_rsa"},
		{"ssh ed25519 key by name", "/Users/eric/.ssh/id_ed25519"},
		{"sibling of ssh key", "/Users/eric/.ssh/known_hosts"},
		{"aws credentials directory", "/Users/eric/.aws/credentials"},
		{"gcloud config", "/Users/eric/.config/gcloud/application_default_credentials.json"},
		{"gh config", "/Users/eric/.config/gh/hosts.yml"},
		{"docker config", "/Users/eric/.docker/config.json"},
		{"PEM in repo", "/repo/app/certs/tls.pem"},
		{"PKCS#12 keystore", "/repo/keys/store.p12"},
		{"GPG ascii armored", "/Users/eric/keys/release-signing.asc"},
		{"service-account-*.json", "/repo/secrets/service-account-prod.json"},
		{"credentials.json basename", "/repo/app/credentials.json"},
		{"terraform state", "/repo/infra/terraform.tfstate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := IsSensitivePath(tc.path)
			if !ok {
				t.Fatalf("expected %q to be classified sensitive", tc.path)
			}
			if reason == "" {
				t.Fatalf("expected non-empty reason for %q", tc.path)
			}
		})
	}
}

func TestIsSensitivePathLeavesOrdinaryFilesAlone(t *testing.T) {
	cases := []string{
		"/repo/app/README.md",
		"/repo/app/main.go",
		"/repo/app/locales/en.key", // bare .key extension — false positive avoided
		"/repo/app/.gitignore",
		"/repo/app/src/.eslintrc",
		"/repo/app/.env.example",              // committed template
		"/repo/app/.env.sample",               // committed template
		"/repo/app/config/client.env.example", // not even a .env variant
		"/repo/app/docs/.config/notes.md",     // .config alone isn't sensitive
		"",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			if reason, ok := IsSensitivePath(path); ok {
				t.Fatalf("expected %q to be non-sensitive, got %q", path, reason)
			}
		})
	}
}

func TestIsSensitivePathHandlesRelativeAndDirtyPaths(t *testing.T) {
	cases := []string{
		".env",
		"./.env.local",
		"sub/dir/../.ssh/id_rsa",
		"  /repo/app/.env  ",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			if _, ok := IsSensitivePath(path); !ok {
				t.Fatalf("expected %q to be sensitive after cleaning", path)
			}
		})
	}
}

func TestFindSensitiveTokenInArgsFindsSshKey(t *testing.T) {
	tok, reason, ok := FindSensitiveTokenInArgs([]string{"cat", "~/.ssh/id_rsa"})
	if !ok {
		t.Fatal("expected ~/.ssh/id_rsa to be flagged")
	}
	if tok == "" || reason == "" {
		t.Fatalf("expected non-empty token+reason, got token=%q reason=%q", tok, reason)
	}
}

func TestFindSensitiveTokenInArgsFindsOptionValuePath(t *testing.T) {
	tok, reason, ok := FindSensitiveTokenInArgs([]string{"rg", "--ignore-file=.env"})
	if !ok {
		t.Fatal("expected --ignore-file=.env to be flagged")
	}
	if tok != "--ignore-file=.env" || reason == "" {
		t.Fatalf("expected original token and reason, got token=%q reason=%q", tok, reason)
	}
}

func TestFindSensitiveTokenInArgsIgnoresBenignTokens(t *testing.T) {
	if _, _, ok := FindSensitiveTokenInArgs([]string{"ls", "-la", "/tmp"}); ok {
		t.Fatal("expected benign args to be ignored")
	}
}

func TestSensitivePathReasonMentionsCategory(t *testing.T) {
	reason, ok := IsSensitivePath("/Users/eric/.ssh/id_rsa")
	if !ok {
		t.Fatal("expected ssh key to be sensitive")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}
