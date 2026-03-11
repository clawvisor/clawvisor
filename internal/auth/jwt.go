package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	pkgauth "github.com/clawvisor/clawvisor/pkg/auth"
)

// Claims is re-exported from pkg/auth so that the JWTService methods satisfy
// the TokenService interface (same return type).
type Claims = pkgauth.Claims

// JWTService handles token creation and validation.
// It implements the TokenService interface.
type JWTService struct {
	secret []byte
}

func NewJWTService(secret string) (*JWTService, error) {
	if secret == "" {
		return nil, errors.New("JWT secret must not be empty")
	}
	return &JWTService{secret: []byte(secret)}, nil
}

// GenerateAccessToken creates a short-lived access token.
func (s *JWTService) GenerateAccessToken(userID, email string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   userID,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// GeneratePurposeToken creates a JWT with a purpose restriction.
// Purpose-restricted tokens are rejected by the general RequireUser middleware
// and only accepted by specific endpoints that check for the purpose.
func (s *JWTService) GeneratePurposeToken(userID, email, purpose string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:  userID,
		Email:   email,
		Purpose: purpose,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   userID,
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// ValidateToken parses and validates a JWT, returning its claims.
func (s *JWTService) ValidateToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token claims")
	}
	return claims, nil
}
