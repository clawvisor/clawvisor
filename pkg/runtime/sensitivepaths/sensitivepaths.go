// Package sensitivepaths classifies filesystem paths whose contents
// are likely to be secrets (SSH keys, cloud credentials, .env files,
// signing material, …). Callers use it to keep the LLM proxy's
// permissive default tool-allow paths from silently exposing those
// files: a positive match should fall through to task-scope matching
// and human approval, not be auto-allowed.
//
// The classifier is intentionally conservative on the side of asking:
// false positives only trigger an approval prompt, while false
// negatives leak a secret.
package sensitivepaths

import (
	"path/filepath"
	"strings"
)

// IsSensitivePath reports whether path points at a file whose
// contents are likely a secret. The matched returned string describes
// which pattern hit, suitable for inclusion in a user-facing reason.
func IsSensitivePath(path string) (matched string, ok bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	cleaned := filepath.Clean(path)
	base := filepath.Base(cleaned)
	if reason, ok := matchSensitiveBasename(base); ok {
		return reason, true
	}
	parts := splitPathComponents(cleaned)
	for i, segment := range parts {
		if reason, ok := matchSensitiveDirComponent(segment); ok {
			return reason, true
		}
		// Two-segment directory anchors like .config/gcloud or .config/gh.
		if i+1 < len(parts) {
			pair := segment + "/" + parts[i+1]
			if reason, ok := sensitiveDirPairs[pair]; ok {
				return reason, true
			}
		}
	}
	return "", false
}

func matchSensitiveBasename(base string) (string, bool) {
	if base == "" {
		return "", false
	}
	if envTemplateBasenames[base] {
		// .env.example and friends are committed templates that name
		// the keys without holding real secrets. Don't make the agent
		// ask just to read them.
		return "", false
	}
	if reason, ok := sensitiveBasenames[base]; ok {
		return reason, true
	}
	for prefix, reason := range sensitiveBasenamePrefixes {
		if strings.HasPrefix(base, prefix) {
			return reason, true
		}
	}
	lower := strings.ToLower(base)
	for suffix, reason := range sensitiveBasenameSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return reason, true
		}
	}
	return "", false
}

var envTemplateBasenames = map[string]bool{
	".env.example":  true,
	".env.sample":   true,
	".env.template": true,
	".env.dist":     true,
	".env.defaults": true,
}

func matchSensitiveDirComponent(segment string) (string, bool) {
	if reason, ok := sensitiveDirComponents[segment]; ok {
		return reason, true
	}
	return "", false
}

func splitPathComponents(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	// filepath.Clean on POSIX paths leaves "/" as separator; on Windows
	// it normalizes to backslash. The proxy runs on Linux/macOS, but be
	// defensive: split on both.
	replaced := strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(replaced, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Exact-basename matches. These are filenames whose entire purpose is
// to hold credentials or secrets; reading them should never be silent.
var sensitiveBasenames = map[string]string{
	".env":                     ".env file",
	".envrc":                   ".envrc file",
	".netrc":                   ".netrc credentials",
	".npmrc":                   ".npmrc (often contains auth tokens)",
	".pypirc":                  ".pypirc (PyPI credentials)",
	".boto":                    ".boto (AWS credentials)",
	".pgpass":                  ".pgpass (Postgres password file)",
	".my.cnf":                  ".my.cnf (MySQL credentials)",
	"credentials":              "credentials file",
	"credentials.json":         "credentials.json",
	"service-account.json":     "service-account.json",
	"id_rsa":                   "SSH private key (id_rsa)",
	"id_dsa":                   "SSH private key (id_dsa)",
	"id_ecdsa":                 "SSH private key (id_ecdsa)",
	"id_ed25519":               "SSH private key (id_ed25519)",
	"terraform.tfstate":        "terraform.tfstate (may contain secrets)",
	"terraform.tfstate.backup": "terraform.tfstate.backup",
}

// Basename prefixes. e.g. .env.local, .env.production, service-account-foo.json.
var sensitiveBasenamePrefixes = map[string]string{
	".env.":            "dotenv variant",
	"service-account-": "service-account key",
}

// Basename suffixes (case-insensitive). Crypto / key material file
// extensions. We deliberately skip `.key` alone — too noisy (i18n
// files, etc.) — and rely on directory + filename matches for SSH keys.
var sensitiveBasenameSuffixes = map[string]string{
	".pem":      "PEM-encoded key/cert",
	".p12":      "PKCS#12 keystore",
	".pfx":      "PKCS#12 keystore",
	".jks":      "Java keystore",
	".keystore": "Java keystore",
	".asc":      "PGP/GPG armored key",
}

// Directory components: if a path's resolved components include any
// of these, the file is treated as sensitive. Matches the directory
// itself or anything under it.
var sensitiveDirComponents = map[string]string{
	".ssh":         "under ~/.ssh",
	".aws":         "under ~/.aws",
	".gnupg":       "under ~/.gnupg",
	".kube":        "under ~/.kube",
	".gcp":         "under ~/.gcp",
	".docker":      "under ~/.docker (config.json holds registry auths)",
	".terraform.d": "under ~/.terraform.d",
}

// Two-segment ancestor anchors. Useful for paths like ~/.config/gcloud/...
// where ".config" by itself is not sensitive but ".config/gcloud" is.
var sensitiveDirPairs = map[string]string{
	".config/gcloud": "under ~/.config/gcloud",
	".config/gh":     "under ~/.config/gh",
	".config/glab":   "under ~/.config/glab",
	".config/op":     "under ~/.config/op (1Password CLI)",
}

// FindSensitiveTokenInArgs scans an argv-like list (tokens already
// split by a shell parser) and returns the first token that classifies
// as a sensitive path. Returns "" / false if none match.
//
// Used by the read-only shell-command pass-through: even when the
// command is structurally safe (no command substitution, allowlisted
// binaries only), `cat ~/.ssh/id_rsa` still exposes secrets and should
// fall through to task-scope matching and intent verification.
func FindSensitiveTokenInArgs(args []string) (token, reason string, ok bool) {
	for _, arg := range args {
		candidate := normalizeShellPathArg(arg)
		if candidate == "" {
			continue
		}
		if reason, ok := IsSensitivePath(candidate); ok {
			return arg, reason, true
		}
	}
	return "", "", false
}

// normalizeShellPathArg expands a leading ~ (the most common form by
// far), trims surrounding quotes left over from incomplete shell
// parsing, and handles option values like --env-file=.env. We
// deliberately do NOT expand $VAR — callers that care about shell
// expansions should pass the rendered shell token through this same
// classifier so literal sensitive components still match.
func normalizeShellPathArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	if (strings.HasPrefix(arg, "\"") && strings.HasSuffix(arg, "\"") && len(arg) >= 2) ||
		(strings.HasPrefix(arg, "'") && strings.HasSuffix(arg, "'") && len(arg) >= 2) {
		arg = arg[1 : len(arg)-1]
	}
	if strings.HasPrefix(arg, "-") {
		if _, value, ok := strings.Cut(arg, "="); ok {
			arg = strings.TrimSpace(value)
		}
	}
	if strings.HasPrefix(arg, "~/") {
		arg = arg[2:]
	} else if arg == "~" {
		arg = ""
	}
	return arg
}
