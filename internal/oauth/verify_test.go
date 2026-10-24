package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAcceptAllowsMailboxOwner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-key"
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))
	t.Cleanup(jwks.Close)

	const issuer = "https://auth.example/application/o/roundcube/"
	checker, err := New(Config{
		Issuer:   issuer,
		JWKSURL:  jwks.URL,
		Audience: "roundcube",
		Allowed:  []string{"denis", "denis@generic.business", "denis@lemire.name"},
	})
	if err != nil {
		t.Fatal(err)
	}

	token := sign(t, key, kid, jwt.MapClaims{
		"iss":                issuer,
		"aud":                "roundcube",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"preferred_username": "denis",
		"email":              "denis@lemire.name",
	})
	if err := checker.Accept("denis@generic.business", token); err != nil {
		t.Fatal(err)
	}
	if err := checker.Accept("someone-else", token); err == nil {
		t.Fatal("sasl username outside the allow list was accepted")
	}

	other := sign(t, key, kid, jwt.MapClaims{
		"iss":                issuer,
		"aud":                "roundcube",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"preferred_username": "natalie",
		"email":              "natalie@generic.business",
	})
	if err := checker.Accept("denis", other); err == nil {
		t.Fatal("token for another identity was accepted")
	}
}

func TestNewDisabledWithoutJWKS(t *testing.T) {
	checker, err := New(Config{})
	if err != nil || checker != nil {
		t.Fatalf("checker=%v err=%v", checker, err)
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestTokenValidationBoundaries(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		change            func(jwt.MapClaims)
		wrongKey, allowed bool
	}{
		{name: "valid", allowed: true},
		{name: "missing expiration", change: func(c jwt.MapClaims) { delete(c, "exp") }},
		{name: "expired", change: func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{name: "future not-before", change: func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Minute).Unix() }},
		{name: "wrong issuer", change: func(c jwt.MapClaims) { c["iss"] = "other" }},
		{name: "wrong audience", change: func(c jwt.MapClaims) { c["aud"] = "other" }},
		{name: "wrong signature", wrongKey: true},
		{name: "wrong identity", change: func(c jwt.MapClaims) { c["preferred_username"] = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker, err := New(Config{Issuer: "issuer", Audience: "mail", JWKSURL: "https://unused.example/keys", Allowed: []string{"owner"}})
			if err != nil {
				t.Fatal(err)
			}
			checker.keys = map[string]*rsa.PublicKey{"key": &key.PublicKey}
			checker.fetched = time.Now()
			claims := jwt.MapClaims{"iss": "issuer", "aud": "mail", "exp": time.Now().Add(time.Hour).Unix(), "preferred_username": "owner"}
			if tc.change != nil {
				tc.change(claims)
			}
			signingKey := key
			if tc.wrongKey {
				signingKey = other
			}
			err = checker.Accept("owner", sign(t, signingKey, "key", claims))
			if (err == nil) != tc.allowed {
				t.Fatalf("accepted=%v: %v", err == nil, err)
			}
		})
	}
}
