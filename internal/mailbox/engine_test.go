package mailbox

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"

	"github.com/emersion/go-imap/v2"
)

func TestForEachCallbackCanReadBody(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	raw := []byte("From: a@example.com\r\nTo: denis@generic.business\r\nSubject: hello\r\nDate: Tue, 21 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n")
	store.PutMeta("inbound/one", raw, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	eng, err := New(ctx, store, t.TempDir())
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

	done := make(chan struct{})
	go func() {
		var set imap.SeqSet
		set.AddNum(1)
		box.ForEach(set, func(m Msg) {
			got, err := eng.Body(ctx, m.Loc, m.Key)
			if err != nil {
				t.Errorf("body: %v", err)
			}
			if string(got) != string(raw) {
				t.Errorf("body mismatch")
			}
			close(done)
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ForEach callback blocked on the mailbox lock")
	}
}

func TestSyncAssignsStableUID(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	dir := t.TempDir()
	raw := []byte("From: a@example.com\r\nTo: denis@generic.business\r\nSubject: hello\r\nDate: Tue, 21 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n")
	store.PutMeta("inbound/one", raw, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	eng, err := New(ctx, store, dir)
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

	eng2, err := New(ctx, store, dir)
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

func TestImportLegacyIndex(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	raw := []byte("From: a@example.com\r\nTo: denis@generic.business\r\nSubject: kept\r\nDate: Tue, 21 Oct 2026 12:00:00 +0000\r\n\r\nbody\r\n")
	sent := []byte("From: denis@generic.business\r\nTo: a@example.com\r\nSubject: sent\r\nDate: Tue, 21 Oct 2026 13:00:00 +0000\r\n\r\nout\r\n")
	store.PutMeta("inbound/one", raw, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	store.PutMeta("inbound/notice", []byte("amazon_ses_setup_notification"), time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC))
	store.PutMeta("sent/old", sent, time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC))
	legacy := []byte(`{
	  "boxes": {
	    "INBOX": {
	      "uidValidity": 424242,
	      "uidNext": 739,
	      "subscribed": true,
	      "messages": [
	        {"uid": 12, "key": "inbound/one", "size": 10, "internal": "2026-10-21T12:00:00Z", "flags": ["\\Seen","\\Flagged"]}
	      ]
	    },
	    "Sent": {
	      "uidValidity": 7,
	      "uidNext": 4,
	      "subscribed": true,
	      "messages": [
	        {"uid": 3, "key": "sent/old", "size": 20, "internal": "2026-10-21T13:00:00Z", "flags": ["\\Seen"], "owned": true}
	      ]
	    }
	  },
	  "skipped": ["inbound/notice"]
	}`)
	store.PutMeta(blob.StateKey, legacy, time.Now().UTC())
	before, _, err := store.GetState(ctx, blob.StateKey)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	eng, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := store.GetState(ctx, blob.StateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("import rewrote the S3 index")
	}

	box, err := eng.Open("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	st := box.SelectData()
	if st.UIDValidity != 424242 || st.UIDNext != 739 {
		t.Fatalf("inbox validity: %+v", st)
	}
	msgs := box.Messages()
	if len(msgs) != 1 || msgs[0].UID != 12 || msgs[0].Loc != locS3 || msgs[0].Key != "inbound/one" {
		t.Fatalf("inbox: %+v", msgs)
	}
	if !hasFlag(msgs[0].Flags, imap.FlagSeen) || !hasFlag(msgs[0].Flags, imap.FlagFlagged) {
		t.Fatalf("flags: %+v", msgs[0].Flags)
	}
	body, err := eng.Body(ctx, msgs[0].Loc, msgs[0].Key)
	if err != nil || string(body) != string(raw) {
		t.Fatalf("inbox body: %v %q", err, body)
	}

	sentBox, err := eng.Open("Sent")
	if err != nil {
		t.Fatal(err)
	}
	sentMsgs := sentBox.Messages()
	if len(sentMsgs) != 1 || sentMsgs[0].UID != 3 || sentMsgs[0].Key != "sent/old" {
		t.Fatalf("sent: %+v", sentMsgs)
	}
	if got, err := eng.Body(ctx, sentMsgs[0].Loc, sentMsgs[0].Key); err != nil || string(got) != string(sent) {
		t.Fatalf("sent body: %v", err)
	}

	if err := eng.SyncInbound(ctx, "inbound/"); err != nil {
		t.Fatal(err)
	}
	if n := len(box.Messages()); n != 1 {
		t.Fatalf("sync resurrected or duplicated: %d", n)
	}

	keysBefore := storeKeys(t, store)
	if err := eng.Create(ctx, "Archive"); err != nil {
		t.Fatal(err)
	}
	var set imap.UIDSet
	set.AddNum(12)
	if _, err := box.Copy(ctx, set, "Archive"); err != nil {
		t.Fatal(err)
	}
	if extra := newKeys(keysBefore, storeKeys(t, store)); len(extra) != 0 {
		t.Fatalf("copy wrote S3 objects: %v", extra)
	}
	arch, err := eng.Open("Archive")
	if err != nil {
		t.Fatal(err)
	}
	copied := arch.Messages()
	if len(copied) != 1 || copied[0].Loc != locFile {
		t.Fatalf("archive: %+v", copied)
	}
	if got, err := eng.Body(ctx, copied[0].Loc, copied[0].Key); err != nil || string(got) != string(raw) {
		t.Fatalf("copied body: %v", err)
	}

	box.Store(imap.UIDSet{{Start: 12, Stop: 12}}, imap.StoreFlagsAdd, []imap.Flag{imap.FlagDeleted}, nil)
	if err := box.Expunge(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "inbound/one"); err != nil {
		t.Fatalf("expunge removed inbound object: %v", err)
	}
	if _, err := store.Get(ctx, "sent/old"); err != nil {
		t.Fatalf("expunge removed sent object: %v", err)
	}
	eng2, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng2.SyncInbound(ctx, "inbound/"); err != nil {
		t.Fatal(err)
	}
	inbox2, _ := eng2.Open("INBOX")
	if n := len(inbox2.Messages()); n != 0 {
		t.Fatalf("expunged inbound came back: %d", n)
	}
	sent2, _ := eng2.Open("Sent")
	again := sent2.Messages()
	if len(again) != 1 || again[0].UID != 3 || sent2.SelectData().UIDValidity != 7 {
		t.Fatalf("sent did not survive restart: %+v validity %d", again, sent2.SelectData().UIDValidity)
	}
	arch2, _ := eng2.Open("Archive")
	if n := len(arch2.Messages()); n != 1 || arch2.Messages()[0].Loc != locFile {
		t.Fatalf("archive did not survive restart: %+v", arch2.Messages())
	}
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, f := range flags {
		if canonicalFlag(f) == canonicalFlag(want) {
			return true
		}
	}
	return false
}

func storeKeys(t *testing.T, store *blob.Mem) map[string]struct{} {
	t.Helper()
	metas, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]struct{}{}
	for _, m := range metas {
		out[m.Key] = struct{}{}
	}
	return out
}

func newKeys(before, after map[string]struct{}) []string {
	var extra []string
	for k := range after {
		if _, ok := before[k]; !ok {
			extra = append(extra, k)
		}
	}
	return extra
}
