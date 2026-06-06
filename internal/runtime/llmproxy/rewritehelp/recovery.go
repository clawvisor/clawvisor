package rewritehelp

import (
	"errors"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
)

func CredentialedRewriteRecoveryReason(v inspector.Verdict, err error) string {
	if err == nil {
		return "Clawvisor: rewriter refused"
	}
	if errors.Is(err, inspector.ErrNoRewriter) {
		var b strings.Builder
		b.WriteString("Clawvisor: detected credentialed API access, but this tool shape cannot be rewritten. ")
		b.WriteString("Detected ")
		b.WriteString(firstNonEmpty(v.Method, "HTTP"))
		if v.Host != "" {
			b.WriteString(" ")
			b.WriteString(v.Host)
		}
		if v.Path != "" {
			b.WriteString(v.Path)
		}
		if len(v.CredentialLocations) > 0 || len(v.Placeholders) > 0 {
			b.WriteString(" using an autovault placeholder")
		}
		b.WriteString(". Recover by minting a script session: POST ")
		b.WriteString("https://" + controltool.ControlSyntheticHost + controltool.ControlSyntheticPath + "/autovault/script-session")
		host := v.Host
		if host == "" {
			host = "<target host>"
		}
		method := v.Method
		if method == "" {
			method = "GET"
		}
		b.WriteString(" with `{placeholder, target_host:\"")
		b.WriteString(host)
		b.WriteString("\", methods:[\"")
		b.WriteString(method)
		b.WriteString("\"], path_prefixes:[<service-specific prefix covering ")
		if v.Path != "" {
			b.WriteString(v.Path)
		} else {
			b.WriteString("the requests you are making")
		}
		b.WriteString(">], max_uses, ttl_seconds, why}` (hard limits: TTL ≤ 120s, max_uses ≤ 200, GET-only initially). ")
		b.WriteString("Then from your script call `base_url + <upstream path>` with `X-Clawvisor-Caller: Bearer <caller_token>` and `Authorization: Bearer <placeholder>` on each request. ")
		b.WriteString("See GET ")
		b.WriteString("https://" + controltool.ControlSyntheticHost + controltool.ControlSyntheticPath + "/autovault/script")
		b.WriteString(" for the full request shape and error recovery codes.")
		return b.String()
	}
	return "Clawvisor: rewriter refused — " + err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
