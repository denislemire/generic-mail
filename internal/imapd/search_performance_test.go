package imapd

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/emersion/go-imap/v2"
)

type countingStore struct {
	*blob.Mem
	gets    atomic.Int64
	offline bool
	only    string
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets.Add(1)
	if s.offline || (s.only != "" && s.only != key) {
		return nil, errors.New("unexpected remote body read")
	}
	return s.Mem.Get(ctx, key)
}
func TestMetadataSearchAvoidsContentReads(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Mem: blob.NewMem()}
	dir := t.TempDir()
	for _, key := range []string{"inbound/a", "inbound/b"} {
		store.PutMeta(key, []byte("Subject: one\r\n\r\nbody"), time.Now())
	}
	e, err := mailbox.New(ctx, store, dir, mailbox.Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// A new engine has neither RAM nor disk bodies. Metadata must work with S3 down.
	e, err = mailbox.New(ctx, store, dir, mailbox.Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	store.gets.Store(0)
	store.offline = true
	c := testClient(t, e, true)
	for _, criteria := range []*imap.SearchCriteria{
		{}, {NotFlag: []imap.Flag{imap.FlagSeen}}, {UID: []imap.UIDSet{imap.UIDSetNum(1)}},
		{Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}}},
		{Or: [][2]imap.SearchCriteria{{{NotFlag: []imap.Flag{imap.FlagSeen}}, {Body: []string{"unavailable"}}}}},
	} {
		data, err := c.UIDSearch(criteria, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if len(data.AllUIDs()) == 0 {
			t.Fatal("metadata search lost matches")
		}
	}
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("metadata search downloaded %d bodies", got)
	}
}
func TestIndexedSearchReadsOnlyCandidates(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Mem: blob.NewMem()}
	dir := t.TempDir()
	for key, raw := range map[string]string{"inbound/a": "Subject: Invoice\r\n\r\nunique billing text", "inbound/b": "Subject: other\r\n\r\nnot relevant", "inbound/c": "Subject: noise\r\n\r\nmore irrelevant"} {
		store.PutMeta(key, []byte(raw), time.Now())
	}
	e, err := mailbox.New(ctx, store, dir, mailbox.Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	e, err = mailbox.New(ctx, store, dir, mailbox.Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	store.only = "inbound/a"
	store.gets.Store(0)
	c := testClient(t, e, true)
	data, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "invoice"}}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.AllUIDs()) != 1 || store.gets.Load() != 1 {
		t.Fatalf("matched=%d remote reads=%d", len(data.AllUIDs()), store.gets.Load())
	}
	data, err = c.UIDSearch(&imap.SearchCriteria{Text: []string{"nonexistent-query"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.AllUIDs()) != 0 || store.gets.Load() != 1 {
		t.Fatal("negative search fetched unrelated messages")
	}
}
func TestSearchAndFetchUsePersistentCacheWithS3Offline(t *testing.T) {
	ctx := context.Background()
	store := &countingStore{Mem: blob.NewMem()}
	dir := t.TempDir()
	store.PutMeta("inbound/a", []byte("Subject: Invoice\r\n\r\nunique billing text"), time.Now())
	e, err := mailbox.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	e, err = mailbox.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	store.offline = true
	store.gets.Store(0)
	e.WarmContent(ctx)
	c := testClient(t, e, true)
	for _, text := range []string{"invoice", "INVOICE", "bi", "billing", "not present"} {
		data, err := c.UIDSearch(&imap.SearchCriteria{Text: []string{text}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		want := uint32(1)
		if text == "not present" {
			want = 0
		}
		if uint32(len(data.AllUIDs())) != want {
			t.Fatalf("search %q=%d", text, len(data.AllUIDs()))
		}
	}
	fetched, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Envelope: true, BodyStructure: &imap.FetchItemBodyStructure{}, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || !reflect.DeepEqual(fetched[0].Envelope.Subject, "Invoice") {
		t.Fatal("cached fetch changed envelope")
	}
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("restart caused %d remote reads", got)
	}
}
