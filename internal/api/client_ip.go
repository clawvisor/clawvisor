package api

import (
	"net"
	"net/http"
	"strings"
)

// clientIPFromRequest returns the rate-limit key for a request: the client
// IP as best as the daemon can identify it.
//
// When the immediate peer (r.RemoteAddr) is in trustedProxies, the
// X-Forwarded-For header is consulted: walk right-to-left and return the
// right-most entry that is NOT a trusted proxy. This gives the actual
// originating client even with multiple proxy hops, while ensuring an
// attacker on a direct connection can't spoof XFF (because their RemoteAddr
// won't be in trustedProxies in the first place).
//
// When trustedProxies is empty (self-host / direct exposure), only
// r.RemoteAddr is consulted — no XFF spoofing surface.
func clientIPFromRequest(r *http.Request, trustedProxies []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(trustedProxies) == 0 {
		return host
	}
	peerIP := net.ParseIP(host)
	if peerIP == nil || !ipInAny(peerIP, trustedProxies) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		ip := net.ParseIP(candidate)
		if ip == nil {
			continue
		}
		if !ipInAny(ip, trustedProxies) {
			return candidate
		}
	}
	return host
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
