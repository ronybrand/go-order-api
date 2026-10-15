// Package auth verifies the bearer tokens of the API. The service only verifies tokens, it never issues
// them: it holds the public half of the issuer's RS256 key pair, so leaking it grants no ability to mint one.
package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The roles the API knows: authenticated user versus admin, nothing finer.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

const (
	clockSkew        = 30 * time.Second
	maxSubjectLength = 200 // created_by/updated_by/deleted_by are varchar(200)
)

// ErrInvalidToken is returned for every token that cannot be trusted, whatever the reason: neither the caller
// nor the client learns why a token failed.
var ErrInvalidToken = errors.New("invalid token")

// Claims is what the API needs from a verified token.
type Claims struct {
	Subject string
	Roles   []string
}

// HasRole reports whether the token carries the role.
func (c Claims) HasRole(role string) bool {
	return slices.Contains(c.Roles, role)
}

// Verifier validates RS256 tokens against one public key, one audience and one issuer.
type Verifier struct {
	key    *rsa.PublicKey
	parser *jwt.Parser
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles"`
}

// NewVerifier builds a verifier from a PEM-encoded RSA public key.
func NewVerifier(publicKeyPEM []byte, audience, issuer string) (*Verifier, error) {
	if audience == "" || issuer == "" {
		return nil, errors.New("audience and issuer are required")
	}
	key, err := jwt.ParseRSAPublicKeyFromPEM(publicKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	parser := jwt.NewParser(
		// Only RS256: an HS256 token "signed" with the (public) key, or alg=none, never validates.
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithAudience(audience),
		jwt.WithIssuer(issuer),
		// Without this a token with no exp would be valid forever.
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(clockSkew),
	)
	return &Verifier{key: key, parser: parser}, nil
}

// LoadVerifier reads the public key from a file (JWT_PUBLIC_KEY_PATH).
func LoadVerifier(path, audience, issuer string) (*Verifier, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's configuration
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	return NewVerifier(pem, audience, issuer)
}

// Verify validates the token (signature, audience, issuer, exp/nbf) and extracts the claims. A token with no
// subject, or whose subject does not fit the audit columns, is rejected; one without roles is a plain user.
func (v *Verifier) Verify(token string) (Claims, error) {
	var parsed tokenClaims
	if _, err := v.parser.ParseWithClaims(token, &parsed, func(*jwt.Token) (any, error) { return v.key, nil }); err != nil {
		return Claims{}, ErrInvalidToken
	}
	subject := parsed.Subject
	if strings.TrimSpace(subject) == "" || len(subject) > maxSubjectLength {
		return Claims{}, ErrInvalidToken
	}
	roles := parsed.Roles
	if roles == nil {
		roles = []string{RoleUser}
	}
	return Claims{Subject: subject, Roles: roles}, nil
}
