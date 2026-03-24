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
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

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
// and injects the paired device into the request context.
//
// Header format: Authorization: DeviceHMAC <device_id>:<timestamp>:<hmac_hex>
// HMAC message:  "<method>\n<path>\n<timestamp>\n<sha256_hex(body)>"
func RequireDevice(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			const prefix = "DeviceHMAC "
			if !strings.HasPrefix(authHeader, prefix) {
				http.Error(w, `{"error":"missing DeviceHMAC authorization","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader[len(prefix):], ":", 3)
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
			message := fmt.Sprintf("%s\n%s\n%s\n%x", r.Method, r.URL.Path, tsStr, bodyHash)
			mac := hmac.New(sha256.New, hmacKey)
			mac.Write([]byte(message))
			expectedMAC := hex.EncodeToString(mac.Sum(nil))

			if !hmac.Equal([]byte(providedHMAC), []byte(expectedMAC)) {
				http.Error(w, `{"error":"invalid HMAC signature","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), DeviceContextKey, device)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
