package middleware

import "net/http"

// Security adds standard security headers to every response.
func Security(isLocal bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			// object-src and base-uri are not covered by default-src: CSP3
			// dropped object-src from its fallback chain, and base-uri never
			// had one. Without them a legacy plugin embed can execute and an
			// injected <base> tag can re-point every relative script URL,
			// which defeats the script-src allowlist above.
			//
			// connect-src includes http://localhost:25299 so the dashboard can
			// reach the user's local Clawvisor daemon for agent pairing. It is
			// loopback-scoped and does not permit any third-party origin.
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' http://localhost:25299; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
			if !isLocal {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
