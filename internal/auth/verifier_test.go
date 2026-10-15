package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ronybrand/go-order-api/internal/auth"
)

const (
	audience = "order-api"
	issuer   = "https://idp.example"
)

type fixture struct {
	private *rsa.PrivateKey
	pubPEM  []byte
	v       *auth.Verifier
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	v, err := auth.NewVerifier(pemBytes, audience, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{private: key, pubPEM: pemBytes, v: v}
}

func (f fixture) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(f.private)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// valid returns well-formed claims; a nil override removes the claim.
func valid(overrides jwt.MapClaims) jwt.MapClaims {
	claims := jwt.MapClaims{
		"sub": "user-1", "aud": audience, "iss": issuer,
		"exp": time.Now().Add(time.Hour).Unix(), "roles": []string{auth.RoleAdmin},
	}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	return claims
}

func TestVerify_AcceptsAWellFormedToken(t *testing.T) {
	f := newFixture(t)

	claims, err := f.v.Verify(f.sign(t, valid(nil)))

	if err != nil || claims.Subject != "user-1" || !claims.HasRole(auth.RoleAdmin) || claims.HasRole(auth.RoleUser) {
		t.Errorf("claims=%+v err=%v", claims, err)
	}
}

func TestVerify_RolesDefaultToPlainUser(t *testing.T) {
	f := newFixture(t)

	claims, err := f.v.Verify(f.sign(t, valid(jwt.MapClaims{"roles": nil})))

	if err != nil || !claims.HasRole(auth.RoleUser) || claims.HasRole(auth.RoleAdmin) {
		t.Errorf("claims=%+v err=%v", claims, err)
	}
}

func TestVerify_RejectsUntrustworthyTokens(t *testing.T) {
	f := newFixture(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signWith := func(method jwt.SigningMethod, key any) string {
		token, err := jwt.NewWithClaims(method, valid(nil)).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	tests := map[string]string{
		"garbage":                 "not-a-jwt",
		"empty":                   "",
		"no exp (valid forever)":  f.sign(t, valid(jwt.MapClaims{"exp": nil})),
		"expired beyond the skew": f.sign(t, valid(jwt.MapClaims{"exp": time.Now().Add(-2 * time.Minute).Unix()})),
		"not valid yet":           f.sign(t, valid(jwt.MapClaims{"nbf": time.Now().Add(2 * time.Minute).Unix()})),
		"wrong audience":          f.sign(t, valid(jwt.MapClaims{"aud": "someone-else"})),
		"no audience":             f.sign(t, valid(jwt.MapClaims{"aud": nil})),
		"wrong issuer":            f.sign(t, valid(jwt.MapClaims{"iss": "https://evil.example"})),
		"no issuer":               f.sign(t, valid(jwt.MapClaims{"iss": nil})),
		"no subject":              f.sign(t, valid(jwt.MapClaims{"sub": nil})),
		"blank subject":           f.sign(t, valid(jwt.MapClaims{"sub": "   "})),
		"subject too long":        f.sign(t, valid(jwt.MapClaims{"sub": strings.Repeat("x", 201)})),
		"roles as a string":       f.sign(t, valid(jwt.MapClaims{"roles": "admin"})),
		"roles with a number":     f.sign(t, valid(jwt.MapClaims{"roles": []any{"user", 1}})),
		"signed by another key":   signWith(jwt.SigningMethodRS256, other),
		"alg none":                signWith(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType),
		"HS256 signed with the public key (algorithm confusion)": signWith(jwt.SigningMethodHS256, f.pubPEM),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := f.v.Verify(token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerify_ToleratesAClockThatIsALittleOff(t *testing.T) {
	f := newFixture(t)
	justExpired := f.sign(t, valid(jwt.MapClaims{"exp": time.Now().Add(-10 * time.Second).Unix()}))
	almostValid := f.sign(t, valid(jwt.MapClaims{"nbf": time.Now().Add(10 * time.Second).Unix()}))

	if _, err := f.v.Verify(justExpired); err != nil {
		t.Errorf("token expired 10s ago rejected: %v", err)
	}
	if _, err := f.v.Verify(almostValid); err != nil {
		t.Errorf("token valid in 10s rejected: %v", err)
	}
}

func TestVerify_AcceptsASubjectAtTheAuditColumnLimit(t *testing.T) {
	f := newFixture(t)

	if _, err := f.v.Verify(f.sign(t, valid(jwt.MapClaims{"sub": strings.Repeat("x", 200)}))); err != nil {
		t.Errorf("200-char subject rejected: %v", err)
	}
}

func TestNewVerifier_RejectsBadConfiguration(t *testing.T) {
	f := newFixture(t)

	if _, err := auth.NewVerifier([]byte("not a key"), audience, issuer); err == nil {
		t.Error("garbage key accepted")
	}
	if _, err := auth.NewVerifier(f.pubPEM, "", issuer); err == nil {
		t.Error("empty audience accepted")
	}
	if _, err := auth.NewVerifier(f.pubPEM, audience, ""); err == nil {
		t.Error("empty issuer accepted")
	}
}
