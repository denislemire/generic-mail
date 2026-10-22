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
