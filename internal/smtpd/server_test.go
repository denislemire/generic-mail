package smtpd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type senderFunc func(context.Context, string, []string, []byte) error

func (f senderFunc) SendRaw(ctx context.Context, from string, to []string, raw []byte) error {
	return f(ctx, from, to, raw)
}

type rejected struct{}

func (rejected) Error() string   { return "private provider diagnostic" }
func (rejected) Permanent() bool { return true }

func TestDeliveryErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"temporary", errors.New("private transport diagnostic"), 451}, {"permanent", rejected{}, 550}, {"success", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{srv: &Server{Sender: senderFunc(func(ctx context.Context, from string, to []string, raw []byte) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("delivery has no deadline")
				}
				if from != "owner@example.com" || len(to) != 1 || to[0] != "friend@example.com" || string(raw) != "message" {
					t.Error("delivery arguments changed")
				}
				return tc.err
			})}, from: "owner@example.com", rcpts: []string{"friend@example.com"}}
			err := s.Data(strings.NewReader("message"))
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var smtpErr *smtp.SMTPError
			if !errors.As(err, &smtpErr) || smtpErr.Code != tc.code || strings.Contains(smtpErr.Message, "private") {
				t.Fatalf("unexpected response: %v", err)
			}
		})
	}
}
func TestDeliveryDeadline(t *testing.T) {
	s := &session{srv: &Server{SendTimeout: time.Millisecond, Sender: senderFunc(func(ctx context.Context, _ string, _ []string, _ []byte) error { <-ctx.Done(); return ctx.Err() })}, from: "owner@example.com", rcpts: []string{"friend@example.com"}}
	var smtpErr *smtp.SMTPError
	if err := s.Data(strings.NewReader("message")); !errors.As(err, &smtpErr) || smtpErr.Code != 451 {
		t.Fatalf("deadline response: %v", err)
	}
}
func TestSenderPolicy(t *testing.T) {
	s := &session{srv: &Server{Username: "owner@example.com", AllowFrom: []string{"alias@example.net"}, AllowDomains: []string{"trusted.example"}}}
	for _, tc := range []struct {
		address string
		allowed bool
	}{
		{"owner@example.com", true}, {"alias@example.net", true}, {"any@trusted.example", true},
		{"any@generic.business", false}, {"any@eviltrusted.example", false}, {"bad@@trusted.example", false}, {"Owner <owner@example.com>", false},
	} {
		if got := s.allowed(tc.address); got != tc.allowed {
			t.Errorf("allowed(%q)=%v", tc.address, got)
		}
	}
}
func TestRecipientValidationAndPlainIdentity(t *testing.T) {
	s := &session{srv: &Server{Username: "owner", Password: "password"}, from: "owner@example.com"}
	for _, to := range []string{"bad@@example.com", "Display <friend@example.com>"} {
		if err := s.Rcpt(to, nil); err == nil {
			t.Errorf("accepted %q", to)
		}
	}
	auth, err := s.Auth(sasl.Plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = auth.Next([]byte("other\x00owner\x00password")); err == nil || s.authed {
		t.Fatal("accepted another authorization identity")
	}
}
