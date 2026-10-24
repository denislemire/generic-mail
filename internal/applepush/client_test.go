package applepush

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/state"
	"github.com/golang-jwt/jwt/v5"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.p8")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Topic: "com.apple.mobilemail.push.example.test", TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestAPNsRequestAndResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		reason      string
		gone, retry bool
	}{
		{name: "delivered", status: 200},
		{name: "unregistered", status: 410, gone: true},
		{name: "bad token", status: 400, reason: "BadDeviceToken", gone: true},
		{name: "throttled", status: 429, reason: "TooManyRequests", retry: true},
		{name: "configuration", status: 403, reason: "TopicDisallowed", retry: true},
		{name: "server error", status: 503, reason: "ServiceUnavailable", retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			token := strings.Repeat("ab", 32)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 || r.Method != "POST" || r.URL.Path != "/3/device/"+token {
					t.Error("wrong transport or path")
				}
				if r.Header.Get("apns-topic") != c.Topic() || r.Header.Get("apns-push-type") != "background" || r.Header.Get("apns-priority") != "5" || len(r.Header.Get("apns-collapse-id")) != 64 {
					t.Error("incorrect APNs headers")
				}
				parsed, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("authorization"), "bearer "), func(token *jwt.Token) (any, error) {
					if token.Header["kid"] != c.cfg.KeyID {
						t.Error("wrong key ID")
					}
					return &c.key.PublicKey, nil
				}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(c.cfg.TeamID))
				if err != nil || !parsed.Valid {
					t.Errorf("invalid provider JWT: %v", err)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var payload map[string]map[string]string
				if err = json.Unmarshal(raw, &payload); err != nil || len(payload) != 1 || len(payload["aps"]) != 1 || payload["aps"]["account-id"] != "account" {
					t.Error("payload contained unexpected data")
				}
				w.Header().Set("retry-after", "60")
				w.WriteHeader(tc.status)
				if tc.reason != "" {
					json.NewEncoder(w).Encode(map[string]string{"reason": tc.reason})
				}
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			c.endpoint = server.URL
			c.http = server.Client()
			result, err := c.Send(context.Background(), state.PushJob{AccountID: "account", DeviceToken: token})
			if result.Gone != tc.gone || (err != nil) != tc.retry {
				t.Fatalf("unexpected result: %+v %v", result, err)
			}
			if tc.retry && result.RetryAfter != time.Minute {
				t.Fatal("Retry-After not honored")
			}
		})
	}
}
func TestProviderJWTIsCachedAndRefreshed(t *testing.T) {
	c := testClient(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	first, err := c.authorization()
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.authorization()
	if err != nil || second != first {
		t.Fatal("provider token was not reused")
	}
	now = now.Add(46 * time.Minute)
	renewed, err := c.authorization()
	if err != nil || renewed == first {
		t.Fatal("provider token did not refresh")
	}
}
func TestConfigurationAndRegistrationValidation(t *testing.T) {
	if c, err := New(Config{}); err != nil || c != nil {
		t.Fatal("unset configuration must disable push")
	}
	if _, err := New(Config{Topic: "com.example.another-app"}); err == nil {
		t.Fatal("ordinary app topic accepted")
	}
	token := strings.Repeat("AB", 32)
	normalized, names, err := NormalizeRegistration("account", token, "com.apple.mobilemail", []string{"inbox", "INBOX", "Sent"})
	if err != nil || normalized != strings.ToLower(token) || len(names) != 2 || names[0] != "INBOX" {
		t.Fatal("registration normalization failed")
	}
	for _, bad := range []struct {
		account, token, subtopic string
		names                    []string
	}{
		{"account", token + "/../../other", "com.apple.mobilemail", []string{"INBOX"}},
		{"account\n", token, "com.apple.mobilemail", []string{"INBOX"}},
		{"account", token, "another.app", []string{"INBOX"}},
		{"account", token, "com.apple.mobilemail", []string{"INBOX\r\n"}},
	} {
		if _, _, err := NormalizeRegistration(bad.account, bad.token, bad.subtopic, bad.names); err == nil {
			t.Fatal("invalid registration accepted")
		}
	}
}
func TestTransportErrorDoesNotExposeDeviceToken(t *testing.T) {
	c := testClient(t)
	c.endpoint = "https://127.0.0.1:1"
	c.http.Timeout = 100 * time.Millisecond
	token := strings.Repeat("fa", 32)
	_, err := c.Send(context.Background(), state.PushJob{DeviceToken: token, AccountID: "private-account"})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "private-account") {
		t.Fatalf("unsafe transport error")
	}
}
