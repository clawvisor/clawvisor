package autovault

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type KnownPrefixSpec struct {
	Service string
	Prefix  string
}

var knownPrefixSpecs = []KnownPrefixSpec{
	{Service: "anthropic", Prefix: "sk-ant-"},
	{Service: "github", Prefix: "ghp_"},
	{Service: "github", Prefix: "github_pat_"},
	{Service: "openai", Prefix: "sk-"},
	{Service: "resend", Prefix: "re_"},
	{Service: "slack", Prefix: "xoxb-"},
	{Service: "slack", Prefix: "xoxp-"},
	{Service: "stripe", Prefix: "sk_live_"},
	{Service: "stripe", Prefix: "sk_test_"},
}

func KnownPrefixSpecs() []KnownPrefixSpec {
	out := make([]KnownPrefixSpec, len(knownPrefixSpecs))
	copy(out, knownPrefixSpecs)
	return out
}

func NoiseSubtreeKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "system", "tools", "tool_choice", "response_format", "model", "metadata":
		return true
	default:
		return false
	}
}

func ProtectedStringField(fieldName string) bool {
	switch strings.ToLower(strings.TrimSpace(fieldName)) {
	case "encrypted_content", "encryptedcontent", "signature", "thinking":
		return true
	default:
		return false
	}
}

var contextNoisePrefixes = []string{
	"As you answer the user's questions, you can use the following context:",
	"# claudeMd",
	"Contents of ",
}

func LooksLikeContextNoise(value string) bool {
	if len(value) < 64 {
		return false
	}
	for _, prefix := range contextNoisePrefixes {
		if strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

func LooksLikeProtocolNoise(fieldName, value string) bool {
	field := strings.ToLower(strings.TrimSpace(fieldName))
	switch field {
	case "tool_use_id", "id":
		return strings.HasPrefix(value, "toolu_")
	case "type":
		return strings.HasPrefix(value, "clear_thinking_")
	default:
		return false
	}
}

var harnessMetadataTags = []string{
	"system-reminder",
	"available-deferred-tools",
	"command-name",
	"command-message",
	"local-command-caveat",
}

var harnessMetadataREs = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(harnessMetadataTags))
	for _, tag := range harnessMetadataTags {
		out = append(out, regexp.MustCompile(`(?s)<`+regexp.QuoteMeta(tag)+`(?:\s[^>]*)?>.*?</`+regexp.QuoteMeta(tag)+`>`))
	}
	return out
}()

func StripHarnessMetadataTags(value string) string {
	if value == "" || !strings.Contains(value, "<") {
		return value
	}
	out := value
	for _, re := range harnessMetadataREs {
		out = re.ReplaceAllString(out, "")
	}
	return out
}

var knownProtocolNoisePrefixes = []string{
	"cv-nonce-",
	"toolu_",
	"msg_",
	"req_",
	"chatcmpl_",
	"asst_",
	"thread_",
	"run_",
	"step_",
	"call_",
	"clear_thinking_",
}

var (
	uuidCandidateRe        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	jsIdentifierRe         = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	allCapsConstantRe      = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	bundlerChunkSuffixRe   = regexp.MustCompile(`-[A-Za-z0-9_-]{8}$`)
	fileExtensionInValueRe = regexp.MustCompile(`\.(?:js|ts|tsx|jsx|json|py|go|rs|md|html?|css|ya?ml|toml|lock|map|svg|png|jpg|jpeg|gif|webp|woff2?|ttf|otf|sql|sh|env|txt)$`)
)

func LooksObviouslyNonSecret(candidate string) bool {
	if candidate == "" {
		return false
	}
	for _, prefix := range knownProtocolNoisePrefixes {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	if strings.Contains(candidate, "__") {
		return true
	}
	if uuidCandidateRe.MatchString(candidate) {
		return true
	}
	if fileExtensionInValueRe.MatchString(candidate) {
		return true
	}
	if allCapsConstantRe.MatchString(candidate) {
		return true
	}
	if bundlerChunkSuffixRe.MatchString(candidate) && !strings.Contains(candidate, "_") {
		dashIdx := strings.LastIndex(candidate, "-")
		if dashIdx > 0 && IsKebabIdentifier(candidate[:dashIdx]) {
			return true
		}
	}
	if jsIdentifierRe.MatchString(candidate) && HasMixedCase(candidate) && !HasDigit(candidate) {
		return true
	}
	return false
}

func IsKebabIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

func HasMixedCase(s string) bool {
	hasLower, hasUpper := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			hasLower = true
		} else if c >= 'A' && c <= 'Z' {
			hasUpper = true
		}
		if hasLower && hasUpper {
			return true
		}
	}
	return false
}

func HasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

func PrefixRegexFor(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])(` + regexp.QuoteMeta(prefix) + `[A-Za-z0-9_-]{4,})`)
}

func SplitPrefixRegexMatch(prefix, match string) (string, string) {
	idx := strings.Index(match, prefix)
	if idx < 0 {
		return "", match
	}
	return match[:idx], match[idx:]
}

func HighContextSecretField(fieldName string) bool {
	field := strings.ToLower(strings.TrimSpace(fieldName))
	for _, token := range []string{"api_key", "apikey", "access_token", "token", "authorization", "auth", "secret", "password", "passcode"} {
		if field == token || strings.Contains(field, token) {
			return true
		}
	}
	return false
}

func SecretContextHint(content, candidate string) bool {
	lower := strings.ToLower(content)
	lower = strings.ReplaceAll(lower, candidate, "<candidate>")
	for _, hint := range []string{"api key", "access token", "authorization", "bearer", "password", "secret", "token"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func GuessService(fieldName, content string) string {
	lower := strings.ToLower(fieldName + " " + content)
	for _, spec := range knownPrefixSpecs {
		if strings.Contains(lower, spec.Service) {
			return spec.Service
		}
	}
	return "captured"
}

func NormalizeSecretService(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, ".", "_")
	return value
}

func RedactedCandidateContext(content, candidate string) string {
	replacements := map[string]string{}
	if candidate != "" {
		replacements[candidate] = "<TOKEN_CANDIDATE_1>"
	}
	next := 2
	for _, peer := range DetectCandidates(content) {
		value := strings.TrimSpace(peer.Value)
		if value == "" {
			continue
		}
		if _, ok := replacements[value]; ok {
			continue
		}
		replacements[value] = fmt.Sprintf("<TOKEN_CANDIDATE_%d>", next)
		next++
	}
	out := content
	values := make([]string, 0, len(replacements))
	for value := range replacements {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) == len(values[j]) {
			return values[i] < values[j]
		}
		return len(values[i]) > len(values[j])
	})
	for _, value := range values {
		out = strings.ReplaceAll(out, value, replacements[value])
	}
	return out
}

func AdjudicationCacheKey(host, fieldName, charset, contextWindow string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host) + "\n" + strings.ToLower(fieldName) + "\n" + charset + "\n" + contextWindow))
	return hex.EncodeToString(sum[:])
}
