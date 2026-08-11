package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-jwt-secret-32-bytes-padding!!"

func newTestService(t *testing.T) *JWTService {
	t.Helper()
	s, err := NewJWTService(testSecret)
	if err != nil {
		t.Fatalf("NewJWTService: %v", err)
	}
	return s
}

func TestValidateToken_RoundTrip(t *testing.T) {
	s := newTestService(t)
	tok, err := s.GenerateAccessToken("user-1", "a@b.com", time.Hour)
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	claims, err := s.ValidateToken(tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Fatalf("UserID = %q, want user-1", claims.UserID)
	}
}

// The verifier must accept only the algorithm this service issues. HS384 and
// HS512 verify against the same secret, so a keyfunc that checks the HMAC
// family alone would let them through.
func TestValidateToken_RejectsOtherHMACAlgorithms(t *testing.T) {
	s := newTestService(t)
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
		t.Run(method.Alg(), func(t *testing.T) {
			forged, err := jwt.NewWithClaims(method, &Claims{
				UserID: "attacker",
				RegisteredClaims: jwt.RegisteredClaims{
					ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				},
			}).SignedString([]byte(testSecret))
			if err != nil {
				t.Fatalf("signing %s: %v", method.Alg(), err)
			}
			if _, err := s.ValidateToken(forged); err == nil {
				t.Fatalf("%s token was accepted; only HS256 is issued", method.Alg())
			}
		})
	}
}

// alg=none must never be accepted.
func TestValidateToken_RejectsNone(t *testing.T) {
	s := newTestService(t)
	forged, err := jwt.NewWithClaims(jwt.SigningMethodNone, &Claims{
		UserID: "attacker",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("signing none: %v", err)
	}
	if _, err := s.ValidateToken(forged); err == nil {
		t.Fatal("alg=none token was accepted")
	}
}
