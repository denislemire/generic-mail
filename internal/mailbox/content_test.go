package mailbox

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/emersion/go-imap/v2"
)

type countedBlobs struct {
	*blob.Mem
	gets  atomic.Int64
	delay time.Duration
}

func (b *countedBlobs) Get(ctx context.Context, key string) ([]byte, error) {
	b.gets.Add(1)
	if b.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.delay):
		}
	}
	return b.Mem.Get(ctx, key)
}
func TestConcurrentColdBodyReadsAreCoalesced(t *testing.T) {
	ctx := context.Background()
	store := &countedBlobs{Mem: blob.NewMem()}
	raw := []byte("Subject: one\r\n\r\nbody")
	store.PutMeta("inbound/a", raw, time.Now())
	dir := t.TempDir()
	e, err := New(ctx, store, dir, Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	e, err = New(ctx, store, dir, Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	store.delay = 20 * time.Millisecond
	store.gets.Store(0)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.Body(ctx, locS3, "inbound/a")
			if err != nil || !bytes.Equal(got, raw) {
				t.Error("cold body read failed")
			}
		}()
	}
	wg.Wait()
	if n := store.gets.Load(); n != 1 {
		t.Fatalf("concurrent requests downloaded same object %d times", n)
	}
}
func TestBackfillAndTrashRetirePersistentContent(t *testing.T) {
	ctx := context.Background()
	store := &countedBlobs{Mem: blob.NewMem()}
	raw := []byte("Subject: one\r\n\r\nsearch phrase")
	store.PutMeta("inbound/a", raw, time.Now())
	dir := t.TempDir()
	e, err := New(ctx, store, dir, Options{DiskCacheBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncInbound(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// Enabling the disk cache also backfills bodies whose index already exists.
	e, err = New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	store.gets.Store(0)
	e.WarmContent(ctx)
	if store.gets.Load() != 1 {
		t.Fatal("missing persistent body not backfilled")
	}
	if _, err = e.disk.Get(locS3 + "\ninbound/a"); err != nil {
		t.Fatal(err)
	}
	inbox, _ := e.Open(BoxINBOX)
	copied, err := inbox.Copy(ctx, imap.UIDSetNum(1), BoxTrash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.Store(imap.UIDSetNum(1), imap.StoreFlagsAdd, []imap.Flag{imap.FlagDeleted}, nil); err != nil {
		t.Fatal(err)
	}
	ids := imap.UIDSetNum(1)
	if err = inbox.Expunge(ctx, &ids); err != nil {
		t.Fatal(err)
	}
	trash, _ := e.Open(BoxTrash)
	if _, err = trash.Store(copied.DestUIDs, imap.StoreFlagsAdd, []imap.Flag{imap.FlagDeleted}, nil); err != nil {
		t.Fatal(err)
	}
	if err = trash.Expunge(ctx, &copied.DestUIDs); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, "inbound/a"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("S3 test source survived")
	}
	if _, err = e.disk.Get(locS3 + "\ninbound/a"); err == nil {
		t.Fatal("deleted source survived in persistent cache")
	}
	complete, fallback, err := e.db.SearchStats(ctx)
	if err != nil || complete != 0 || fallback != 0 {
		t.Fatal("deleted search projection survived")
	}
}
