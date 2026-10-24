package imapd

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
)

func testClient(t *testing.T, engine *mailbox.Engine, readOnly bool) *imapclient.Client {
	t.Helper()
	backend := &Server{Engine: engine, Username: "owner", Password: "password"}
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return backend.NewSession(), nil, nil
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	client := imapclient.New(conn, nil)
	t.Cleanup(func() { client.Close(); srv.Close(); <-done })
	if err := client.Login("owner", "password").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: readOnly}).Wait(); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestReadFlags(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		peek, readOnly, wantSeen bool
	}{
		{name: "read", wantSeen: true}, {name: "preview", peek: true}, {name: "examine", readOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := blob.NewMem()
			dir := t.TempDir()
			store.PutMeta("inbound/one", []byte("Subject: hello\r\n\r\nbody\r\n"), time.Now())
			engine, err := mailbox.New(ctx, store, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = engine.SyncInbound(ctx, ""); err != nil {
				t.Fatal(err)
			}
			client := testClient(t, engine, tc.readOnly)
			messages, err := client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{{Peek: tc.peek}}}).Collect()
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) == 0 || seen(messages[0].Flags) != tc.wantSeen {
				t.Fatalf("initial response flags: %+v", messages)
			}
			restarted, err := mailbox.New(ctx, store, dir)
			if err != nil {
				t.Fatal(err)
			}
			box, _ := restarted.Open("INBOX")
			if got := seen(box.Messages()[0].Flags); got != tc.wantSeen {
				t.Fatalf("persisted seen=%v", got)
			}
		})
	}
}

func TestManualReadUnreadWithoutBody(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	raw := []byte("Subject: hello\r\n\r\nbody\r\n")
	store.PutMeta("inbound/one", raw, time.Now())
	dir := t.TempDir()
	engine, err := mailbox.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// Reopening clears the body cache. Flag operations still work without S3.
	engine, err = mailbox.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Delete(ctx, "inbound/one")
	client := testClient(t, engine, false)
	for _, silent := range []bool{true, false} {
		for _, op := range []imap.StoreFlagsOp{imap.StoreFlagsAdd, imap.StoreFlagsDel} {
			if _, err := client.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}, Silent: silent}, nil).Collect(); err != nil {
				t.Fatal(err)
			}
			got, err := client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true, RFC822Size: true}).Collect()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || seen(got[0].Flags) != (op == imap.StoreFlagsAdd) || got[0].RFC822Size != int64(len(raw)) {
				t.Fatalf("unexpected flags/size: %+v", got)
			}
		}
	}
}
func seen(flags []imap.Flag) bool {
	for _, f := range flags {
		if strings.EqualFold(string(f), string(imap.FlagSeen)) {
			return true
		}
	}
	return false
}
