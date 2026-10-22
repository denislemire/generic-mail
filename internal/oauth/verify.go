package oauth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config is the Authentik provider this process will trust.
type Config struct {
	Issuer   string
	JWKSURL  string
	Audience string
	Allowed  []string
}

// Checker validates an access token and the SASL username against one mailbox owner.
type Checker struct {
	issuer   string
	audience string
	jwksURL  string
	allowed  map[string]struct{}
	client   *http.Client

	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	fetched  time.Time
	cacheFor time.Duration
}

// New returns nil when JWKS is unset, which leaves password login as the only mechanism.
func New(cfg Config) (*Checker, error) {
	if cfg.JWKSURL == "" {
		return nil, nil
	}
	if cfg.Issuer == "" || cfg.Audience == "" || len(cfg.Allowed) == 0 {
		return nil, errors.New("OAUTH_ISSUER, OAUTH_AUDIENCE, and OAUTH_ALLOWED are required when OAUTH_JWKS_URL is set")
	}
	allowed := make(map[string]struct{}, len(cfg.Allowed))
	for _, name := range cfg.Allowed {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, errors.New("OAUTH_ALLOWED is empty")
	}
	return &Checker{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		jwksURL:  cfg.JWKSURL,
		allowed:  allowed,
		client:   &http.Client{Timeout: 5 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
		cacheFor: 15 * time.Minute,
	}, nil
}

func (c *Checker) Enabled() bool { return c != nil }

// Accept reports whether this SASL identity presented a token for the mailbox owner.
func (c *Checker) Accept(username, token string) error {
	if c == nil {
		return errors.New("oauth is not configured")
	}
	if !c.allows(username) {
		return errors.New("sasl username is not allowed")
	}
	claims := &idClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, c.key,
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(c.issuer),
		jwt.WithAudience(c.audience),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil || !parsed.Valid {
		return fmt.Errorf("token rejected: %w", err)
	}
	if !c.allows(claims.PreferredUsername) && !c.allows(claims.Email) {
		return errors.New("token identity is not allowed")
	}
	return nil
}

func (c *Checker) allows(name string) bool {
	_, ok := c.allowed[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func (c *Checker) key(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	if kid == "" {
		return nil, errors.New("token has no kid")
	}
	if key, ok := c.cached(kid, false); ok {
		return key, nil
	}
	if err := c.refresh(); err != nil {
		return nil, err
	}
	key, ok := c.cached(kid, true)
	if !ok {
		return nil, errors.New("jwks has no matching key")
	}
	return key, nil
}

func (c *Checker) cached(kid string, ignoreAge bool) (*rsa.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ignoreAge && (c.fetched.IsZero() || time.Since(c.fetched) > c.cacheFor) {
		return nil, false
	}
	key, ok := c.keys[kid]
	return key, ok
}

func (c *Checker) refresh() error {
	resp, err := c.client.Get(c.jwksURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks status %s", resp.Status)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, key := range doc.Keys {
		if key.Kty != "RSA" || key.Kid == "" || key.N == "" || key.E == "" {
			continue
		}
		pub, err := key.public()
		if err != nil {
			return err
		}
		keys[key.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no rsa keys")
	}
	c.mu.Lock()
	c.keys = keys
	c.fetched = time.Now()
	c.mu.Unlock()
	return nil
}

type idClaims struct {
	jwt.RegisteredClaims
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) public() (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("jwks exponent is empty")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
