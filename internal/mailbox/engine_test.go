package mailbox

import (
	"context"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"

	"github.com/emersion/go-imap/v2"
)

func TestSyncAssignsStableUID(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	raw := []byte("From: a@example.com\r\nTo: denis@generic.business\r\nSubject: hello\r\nDate: Tue, 21 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n")
	store.PutMeta("inbound/one", raw, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	eng, err := New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SyncInbound(ctx, "inbound/"); err != nil {
		t.Fatal(err)
	}
	box, err := eng.Open("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	msgs := box.Messages()
	if len(msgs) != 1 || msgs[0].UID != 1 {
		t.Fatalf("messages: %+v", msgs)
	}

	box.Store(imap.UIDSet{{Start: 1, Stop: 1}}, imap.StoreFlagsAdd, []imap.Flag{imap.FlagSeen}, nil)
	if err := eng.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	eng2, err := New(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	box2, _ := eng2.Open("inbox")
	again := box2.Messages()
	if len(again) != 1 || again[0].UID != 1 {
		t.Fatalf("reloaded: %+v", again)
	}
	seen := false
	for _, f := range again[0].Flags {
		if canonicalFlag(f) == canonicalFlag(imap.FlagSeen) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("seen flag missing: %+v", again[0].Flags)
	}

	if err := eng2.SyncInbound(ctx, "inbound/"); err != nil {
		t.Fatal(err)
	}
	if n := len(box2.Messages()); n != 1 {
		t.Fatalf("sync duplicated inbox: %d", n)
	}
}
