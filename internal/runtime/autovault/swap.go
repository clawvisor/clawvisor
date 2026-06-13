package autovault

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/placeholdershape"
)

func ReplaceHeaderValue(value string, resolve func(placeholder string) (string, error)) (string, []string, error) {
	if !HeaderMaybeContainsShadow(value) {
		return value, nil, nil
	}
	if scheme, rest, ok := strings.Cut(value, " "); ok && strings.EqualFold(scheme, "Basic") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return value, nil, nil
		}
		user, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return value, nil, nil
		}
		var replaced []string
		if LooksLikeShadow(user) {
			original := user
			resolved, err := resolve(user)
			if err != nil {
				return "", nil, err
			}
			user = resolved
			replaced = append(replaced, original)
		}
		if LooksLikeShadow(pass) {
			original := pass
			resolved, err := resolve(pass)
			if err != nil {
				return "", nil, err
			}
			pass = resolved
			replaced = append(replaced, original)
		}
		if len(replaced) == 0 {
			return value, nil, nil
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		return "Basic " + encoded, replaced, nil
	}

	matches := placeholdershape.FindAllAutovault(value)
	if len(matches) == 0 {
		return value, nil, nil
	}
	// Sort matches by descending length so a shorter placeholder that
	// is a prefix of a longer one (e.g., `autovault_x_aa` vs
	// `autovault_x_aaa`) doesn't corrupt the longer placeholder when
	// strings.ReplaceAll runs against the same buffer. With 96-bit
	// random suffixes the collision is astronomically unlikely in
	// practice, but the code shape is fragile to future placeholder
	// format changes — sort defensively. Deduplicate while sorting
	// so a value carrying the same placeholder twice doesn't call
	// resolve() twice.
	seen := make(map[string]struct{}, len(matches))
	uniq := matches[:0]
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })

	out := value
	var replaced []string
	for _, candidate := range uniq {
		if !LooksLikeShadow(candidate) {
			continue
		}
		resolved, err := resolve(candidate)
		if err != nil {
			return "", nil, err
		}
		out = strings.ReplaceAll(out, candidate, resolved)
		replaced = append(replaced, candidate)
	}
	return out, replaced, nil
}

func ExtractCredentialValue(credential []byte) (string, error) {
	trimmed := strings.TrimSpace(string(credential))
	if trimmed == "" {
		return "", fmt.Errorf("credential is empty")
	}
	if trimmed[0] != '{' {
		return trimmed, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(credential, &decoded); err != nil {
		return "", fmt.Errorf("parse credential: %w", err)
	}
	for _, key := range []string{"access_token", "token", "api_key", "password"} {
		if raw, ok := decoded[key].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw), nil
		}
	}
	return "", fmt.Errorf("credential has no swappable token field")
}
