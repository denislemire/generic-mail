package smtpd

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/denislemire/generic-mail/internal/oauth"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Sender delivers a raw RFC822 message. SES is the production implementation.
type Sender interface {
	SendRaw(ctx context.Context, from string, rcpts []string, raw []byte) error
}

// Server is an authenticated submission listener that relays to Sender.
type Server struct {
	Hostname     string
	Username     string
	Password     string
	AllowFrom    []string
	AllowDomains []string
	SendTimeout  time.Duration
	Sender       Sender
	MaxBytes     int64
	OAuth        *oauth.Checker
}

func (s *Server) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &session{srv: s}, nil
}

func (s *Server) Serve(ln net.Listener, tlsConfig *tls.Config) error {
	srv := smtp.NewServer(s)
	srv.Domain = s.Hostname
	srv.AllowInsecureAuth = false
	srv.TLSConfig = tlsConfig
	srv.MaxMessageBytes = s.maxBytes()
	srv.MaxRecipients = 50
	return srv.Serve(ln)
}

func (s *Server) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return 30 << 20
}

type session struct {
	srv    *Server
	authed bool
	from   string
	rcpts  []string
}

func (s *session) AuthMechanisms() []string {
	mechs := []string{sasl.Plain}
	if s.srv.OAuth.Enabled() {
		mechs = append(mechs, oauth.XOAuth2, sasl.OAuthBearer)
	}
	return mechs
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch strings.ToUpper(mech) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && !strings.EqualFold(identity, username) {
				return smtp.ErrAuthFailed
			}
			userOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(username)), []byte(strings.ToLower(s.srv.Username))) == 1
			passOK := len(password) == len(s.srv.Password) && subtle.ConstantTimeCompare([]byte(password), []byte(s.srv.Password)) == 1
			if !userOK || !passOK {
				return smtp.ErrAuthFailed
			}
			s.authed = true
			return nil
		}), nil
	case oauth.XOAuth2, sasl.OAuthBearer:
		if !s.srv.OAuth.Enabled() {
			return nil, smtp.ErrAuthUnknownMechanism
		}
		return oauth.NewServer(func(username, token string) error {
			if err := s.srv.OAuth.Accept(username, token); err != nil {
				return smtp.ErrAuthFailed
			}
			s.authed = true
			return nil
		}), nil
	default:
		return nil, smtp.ErrAuthUnknownMechanism
	}
}

func (s *session) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *session) Logout() error { return nil }

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "authentication required"}
	}
	addr := strings.ToLower(strings.Trim(from, "<>"))
	if !s.allowed(addr) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender is not allowed"}
	}
	s.from = addr
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.from == "" {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "MAIL first"}
	}
	addr := strings.Trim(to, "<>")
	if parsed, err := mail.ParseAddress(addr); err != nil || parsed.Address != addr {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "invalid recipient"}
	}
	s.rcpts = append(s.rcpts, addr)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if s.from == "" || len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "incomplete transaction"}
	}
	raw, err := io.ReadAll(io.LimitReader(r, s.srv.maxBytes()+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > s.srv.maxBytes() {
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "message too large"}
	}
	timeout := s.srv.SendTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.srv.Sender.SendRaw(ctx, s.from, s.rcpts, raw); err != nil {
		log.Printf("submission delivery failed: %v", err)
		var permanent interface{ Permanent() bool }
		if errors.As(err, &permanent) && permanent.Permanent() {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "message rejected by delivery service"}
		}
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "delivery temporarily unavailable; please retry"}
	}
	return nil
}

func (s *session) allowed(from string) bool {
	parsed, err := mail.ParseAddress(from)
	if err != nil || parsed.Address != from {
		return false
	}
	if from == strings.ToLower(s.srv.Username) {
		return true
	}
	for _, allowed := range s.srv.AllowFrom {
		if strings.EqualFold(from, strings.TrimSpace(allowed)) {
			return true
		}
	}
	_, domain, ok := strings.Cut(from, "@")
	if !ok {
		return false
	}
	for _, allowed := range s.srv.AllowDomains {
		if strings.EqualFold(domain, strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}
