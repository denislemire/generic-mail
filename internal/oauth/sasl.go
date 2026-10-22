package oauth

import (
	"bytes"
	"errors"
	"strings"

	"github.com/emersion/go-sasl"
)

// Mechanisms Roundcube offers when oauth_auth_type is the default OAUTH.
const (
	XOAuth2 = "XOAUTH2"
)

// AcceptFunc reports whether username may present token.
type AcceptFunc func(username, token string) error

type server struct {
	done   bool
	accept AcceptFunc
}

// NewServer accepts Roundcube's initial response for XOAUTH2 and OAUTHBEARER.
// Roundcube puts the raw access token in auth=, without a Bearer prefix.
func NewServer(accept AcceptFunc) sasl.Server {
	return &server{accept: accept}
}

func (s *server) Next(response []byte) (challenge []byte, done bool, err error) {
	if s.done {
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
	if response == nil {
		return []byte{}, false, nil
	}
	s.done = true
	username, token, err := parseInitial(response)
	if err != nil {
		return nil, true, err
	}
	if err := s.accept(username, token); err != nil {
		return nil, true, err
	}
	return nil, true, nil
}

func parseInitial(raw []byte) (username, token string, err error) {
	if bytes.HasPrefix(raw, []byte("user=")) {
		return parseXOAuth2(raw)
	}
	if bytes.HasPrefix(raw, []byte("n,")) {
		return parseOAuthBearer(raw)
	}
	return "", "", errors.New("oauth: unrecognized sasl response")
}

func parseXOAuth2(raw []byte) (string, string, error) {
	var username, token string
	for _, field := range bytes.Split(raw, []byte{0x01}) {
		if len(field) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(field), "=")
		if !ok {
			return "", "", errors.New("oauth: malformed xoauth2 field")
		}
		switch key {
		case "user":
			username = value
		case "auth":
			token = stripBearer(value)
		}
	}
	if username == "" || token == "" {
		return "", "", errors.New("oauth: incomplete xoauth2 response")
	}
	return username, token, nil
}

func parseOAuthBearer(raw []byte) (string, string, error) {
	parts := bytes.SplitN(raw, []byte{','}, 3)
	if len(parts) != 3 || !bytes.Equal(parts[0], []byte("n")) || !bytes.HasPrefix(parts[1], []byte("a=")) {
		return "", "", errors.New("oauth: malformed oauthbearer response")
	}
	username := string(bytes.TrimPrefix(parts[1], []byte("a=")))
	var token string
	for _, field := range bytes.Split(parts[2], []byte{0x01}) {
		if len(field) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(field), "=")
		if !ok {
			return "", "", errors.New("oauth: malformed oauthbearer field")
		}
		if key == "auth" {
			token = stripBearer(value)
		}
	}
	if username == "" || token == "" {
		return "", "", errors.New("oauth: incomplete oauthbearer response")
	}
	return username, token, nil
}

func stripBearer(value string) string {
	const prefix = "bearer "
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return value[len(prefix):]
	}
	return value
}
