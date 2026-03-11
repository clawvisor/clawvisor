package auth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenService generates and validates user JWTs.
// The open-source JWTService (HS256) implements this interface.
// Cloud deployments can provide alternative implementations (e.g. RS256, multi-tenant).
type TokenService interface {
	GenerateAccessToken(userID, email string, ttl time.Duration) (string, error)
	GeneratePurposeToken(userID, email, purpose string, ttl time.Duration) (string, error)
	ValidateToken(tokenStr string) (*Claims, error)
}

// MagicTokenStore manages single-use magic link tokens.
// The open-source in-memory implementation lives in internal/auth.
// Cloud deployments can provide alternative implementations (e.g. Redis-backed).
type MagicTokenStore interface {
	Generate(userID string) (string, error)
	Validate(token string) (userID string, err error)
	Cleanup()
}

// Claims is the payload stored inside a user JWT.
type Claims struct {
	UserID  string `json:"user_id"`
	Email   string `json:"email"`
	Purpose string `json:"purpose,omitempty"`
	jwt.RegisteredClaims
}
