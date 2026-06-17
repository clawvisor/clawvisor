package middleware

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

var replayCache sync.Map

func init() {
	go evictExpiredReplays()
}

func evictExpiredReplays() {
	for {
		time.Sleep(time.Minute)
		cutoff := time.Now().Add(-2 * deviceTimestampSkew)
		replayCache.Range(func(key, value any) bool {
			if value.(time.Time).Before(cutoff) {
				replayCache.Delete(key)
			}
			return true
		})
	}
}

const (
	// DeviceContextKey is the context key for the authenticated paired device.
	DeviceContextKey contextKey = "device"

	// deviceTimestampSkew is the maximum allowed clock difference for DeviceHMAC auth.
	deviceTimestampSkew = 5 * time.Minute
)

// DeviceFromContext retrieves the authenticated paired device from a request context.
func DeviceFromContext(ctx context.Context) *store.PairedDevice {
	d, _ := ctx.Value(DeviceContextKey).(*store.PairedDevice)
	return d
}

// RequireDevice is middleware that validates a DeviceHMAC authorization header
// and injects the paired device into the request context. Uses in-memory replay
// cache by default. Use RequireDeviceWithReplayCache for multi-instance deployments.
//
// Two authorization-header shapes are accepted:
//
//   - `Authorization: DeviceHMAC <device_id>:<timestamp>:<hmac_hex>` (legacy v1)
//     message: "<method>\n<path>\n<timestamp>\n<sha256_hex(body)>"
//     Query string is NOT covered by the signature. A request bearing a non-
//     empty query under this algorithm is rejected so the gap can never be
//     exploited by a future route that reads `r.URL.Query()`. Clients should
//     upgrade to v2.
//
//   - `Authorization: DeviceHMAC2 <device_id>:<timestamp>:<hmac_hex>` (preferred)
//     message: "v2\n<method>\n<path>\n<raw_query>\n<timestamp>\n<sha256_hex(body)>"
//     Query string is bound into the signature. The "v2" sentinel prefix makes
//     a v1 client's signature unable to validate against a v2 server message
//     even by coincidence on a no-query request.
func RequireDevice(st store.Store) func(http.Handler) http.Handler {
	return RequireDeviceWithReplayCache(st, NewMemoryReplayCache())
}

// RequireDeviceWithReplayCache creates the RequireDevice middleware with
// a custom replay cache implementation (e.g. Redis-backed).
func RequireDeviceWithReplayCache(st store.Store, rc ReplayCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			const (
				prefixV1 = "DeviceHMAC "
				prefixV2 = "DeviceHMAC2 "
			)
			var (
				rest      string
				algorithm int
			)
			switch {
			case strings.HasPrefix(authHeader, prefixV2):
				rest = authHeader[len(prefixV2):]
				algorithm = 2
			case strings.HasPrefix(authHeader, prefixV1):
				rest = authHeader[len(prefixV1):]
				algorithm = 1
			default:
				http.Error(w, `{"error":"missing DeviceHMAC authorization","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Refuse query-bearing requests under the legacy algorithm
			// even before doing any work. The v1 signature didn't cover
			// the query string; honoring such a request would let a
			// network observer rewrite `?param=…` without invalidating
			// the HMAC. Clients hitting a query-using route must use v2.
			if algorithm == 1 && r.URL.RawQuery != "" {
				http.Error(w, `{"error":"DeviceHMAC v1 cannot sign query strings; upgrade to DeviceHMAC2","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(rest, ":", 3)
			if len(parts) != 3 {
				http.Error(w, `{"error":"malformed DeviceHMAC header","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			deviceID, tsStr, providedHMAC := parts[0], parts[1], parts[2]

			// Validate timestamp.
			tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				http.Error(w, `{"error":"invalid timestamp","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}
			diff := time.Since(time.Unix(tsUnix, 0))
			if math.Abs(diff.Seconds()) > deviceTimestampSkew.Seconds() {
				http.Error(w, `{"error":"timestamp out of range","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Read body for HMAC computation, then re-attach.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"failed to read body","code":"BAD_REQUEST"}`, http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			// Load device from store.
			device, err := st.GetPairedDevice(r.Context(), deviceID)
			if err != nil {
				http.Error(w, `{"error":"unknown device","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Compute expected HMAC.
			hmacKey, err := hex.DecodeString(device.DeviceHMACKey)
			if err != nil {
				http.Error(w, `{"error":"internal error","code":"INTERNAL"}`, http.StatusInternalServerError)
				return
			}
			bodyHash := sha256.Sum256(bodyBytes)
			var message string
			if algorithm == 2 {
				// Sentinel "v2" line prevents cross-version collision: a v1
				// signature on a no-query request can never validate against
				// a v2 message because the prefix line is different.
				message = fmt.Sprintf("v2\n%s\n%s\n%s\n%s\n%x", r.Method, r.URL.Path, r.URL.RawQuery, tsStr, bodyHash)
			} else {
				message = fmt.Sprintf("%s\n%s\n%s\n%x", r.Method, r.URL.Path, tsStr, bodyHash)
			}
			mac := hmac.New(sha256.New, hmacKey)
			mac.Write([]byte(message))
			expectedMAC := hex.EncodeToString(mac.Sum(nil))

			if !hmac.Equal([]byte(providedHMAC), []byte(expectedMAC)) {
				http.Error(w, `{"error":"invalid HMAC signature","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			// Replay protection: reject duplicate (device, timestamp, hmac)
			// tuples. We only enter this cache AFTER the HMAC has been
			// verified — otherwise an unauthenticated attacker could flood
			// the cache with bogus (deviceID, ts, randomHMAC) tuples and
			// grow memory unboundedly until the eviction sweep ran. Doing
			// it here means the cache only ever holds tuples that already
			// proved knowledge of the device key.
			cacheKey := deviceID + ":" + tsStr + ":" + providedHMAC
			if rc.Check(cacheKey) {
				http.Error(w, `{"error":"replayed request","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), DeviceContextKey, device)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
