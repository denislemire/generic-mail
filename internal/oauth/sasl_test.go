package oauth

import "testing"

func TestParseRoundcubeInitial(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		user string
		tok  string
	}{
		{
			name: "xoauth2",
			raw:  "user=denis@generic.business\x01auth=header.payload.sig\x01\x01",
			user: "denis@generic.business",
			tok:  "header.payload.sig",
		},
		{
			name: "oauthbearer raw token",
			raw:  "n,a=denis,\x01auth=header.payload.sig\x01\x01",
			user: "denis",
			tok:  "header.payload.sig",
		},
		{
			name: "oauthbearer bearer prefix",
			raw:  "n,a=denis@lemire.name,\x01host=imap.ehws.generic.business\x01auth=Bearer header.payload.sig\x01\x01",
			user: "denis@lemire.name",
			tok:  "header.payload.sig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, tok, err := parseInitial([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if user != tc.user || tok != tc.tok {
				t.Fatalf("got user=%q token=%q", user, tok)
			}
		})
	}
}

func TestParseInitialRejectsGarbage(t *testing.T) {
	if _, _, err := parseInitial([]byte("PLAIN")); err == nil {
		t.Fatal("expected error")
	}
}
