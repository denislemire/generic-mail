// Package applepush delivers Apple Mail wake-up hints through APNs.
package applepush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/denislemire/generic-mail/internal/state"
	"github.com/golang-jwt/jwt/v5"
)

type Config struct{ Topic, TeamID, KeyID, KeyFile string }

type Client struct {
	cfg      Config
	key      *ecdsa.PrivateKey
	http     *http.Client
	endpoint string
	now      func() time.Time
	mu       sync.Mutex
	bearer   string
	issued   time.Time
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)
var mailTopic = regexp.MustCompile(`^com\.apple\.mobilemail\.push\.[A-Za-z0-9][A-Za-z0-9.-]*$`)

// New returns nil when all configuration is unset. A generic application topic
// cannot wake Apple's Mail app; the topic must be provisioned for Mail by Apple.
func New(cfg Config) (*Client, error) {
	if cfg == (Config{}) {
		return nil, nil
	}
	if len(cfg.Topic) > 255 || !mailTopic.MatchString(cfg.Topic) || !identifier.MatchString(cfg.TeamID) || !identifier.MatchString(cfg.KeyID) || cfg.KeyFile == "" {
		return nil, errors.New("Apple push requires a Mail push topic, 10-character team/key IDs, and a .p8 key file")
	}
	pem, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, errors.New("cannot read Apple push key file")
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(pem)
	if err != nil || key.Curve != elliptic.P256() {
		return nil, errors.New("Apple push key must be a P-256 EC private key")
	}
	return &Client{cfg: cfg, key: key, endpoint: "https://api.push.apple.com", now: time.Now, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}}, nil
}
func (c *Client) Topic() string {
	if c == nil {
		return ""
	}
	return c.cfg.Topic
}

func (c *Client) authorization() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.bearer != "" && now.Sub(c.issued) >= 0 && now.Sub(c.issued) < 45*time.Minute {
		return c.bearer, nil
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": c.cfg.TeamID, "iat": now.Unix()})
	token.Header["kid"] = c.cfg.KeyID
	bearer, err := token.SignedString(c.key)
	if err != nil {
		return "", errors.New("cannot sign Apple push authentication token")
	}
	c.bearer, c.issued = bearer, now
	return bearer, nil
}

type Result struct {
	Gone       bool
	RetryAfter time.Duration
}

// Send includes only the opaque account identifier; no message content is sent.
func (c *Client) Send(ctx context.Context, job state.PushJob) (Result, error) {
	token, err := c.authorization()
	if err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(map[string]any{"aps": map[string]string{"account-id": job.AccountID}})
	if err != nil {
		return Result{}, errors.New("cannot encode Apple push hint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/3/device/"+job.DeviceToken, bytes.NewReader(payload))
	if err != nil {
		return Result{}, errors.New("cannot create Apple push request")
	}
	request.Header.Set("authorization", "bearer "+token)
	request.Header.Set("apns-topic", c.cfg.Topic)
	request.Header.Set("apns-push-type", "background")
	request.Header.Set("apns-priority", "5")
	request.Header.Set("apns-expiration", strconv.FormatInt(c.now().Add(time.Hour).Unix(), 10))
	collapse := sha256.Sum256([]byte(job.AccountID))
	request.Header.Set("apns-collapse-id", hex.EncodeToString(collapse[:]))
	request.Header.Set("content-type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// net/http errors include the URL, which contains the device token.
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, errors.New("Apple push transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Result{}, nil
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body)
	if response.StatusCode == http.StatusGone || response.StatusCode == http.StatusBadRequest && body.Reason == "BadDeviceToken" {
		return Result{Gone: true}, nil
	}
	reason := body.Reason
	if len(reason) > 80 || strings.IndexFunc(reason, func(r rune) bool { return r < 'A' || r > 'Z' && r < 'a' || r > 'z' }) >= 0 {
		reason = "Unknown"
	}
	retry := time.Duration(0)
	if seconds, err := strconv.ParseInt(response.Header.Get("retry-after"), 10, 32); err == nil && seconds > 0 {
		retry = time.Duration(seconds) * time.Second
		if retry > time.Hour {
			retry = time.Hour
		}
	}
	return Result{RetryAfter: retry}, fmt.Errorf("Apple push rejected (HTTP %d, %s)", response.StatusCode, reason)
}

// NormalizeRegistration bounds storage and validates URL/header-sensitive data.
func NormalizeRegistration(account, token, subtopic string, mailboxes []string) (string, []string, error) {
	invalid := errors.New("invalid Apple push registration")
	if account == "" || len(account) > 128 || !utf8.ValidString(account) || strings.IndexFunc(account, unicode.IsControl) >= 0 || subtopic != "com.apple.mobilemail" || len(mailboxes) > 32 {
		return "", nil, invalid
	}
	if len(token) < 64 || len(token) > 512 || len(token)%2 != 0 {
		return "", nil, invalid
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", nil, invalid
	}
	var names []string
	seen := map[string]bool{}
	for _, name := range mailboxes {
		if name == "" || len(name) > 200 || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return "", nil, invalid
		}
		if strings.EqualFold(name, "INBOX") {
			name = "INBOX"
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return strings.ToLower(token), names, nil
}
