package mailbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
)

func TestEventAndReconciliationDeduplicateDurably(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	dir := t.TempDir()
	store.PutMeta("inbound/one", []byte("Subject: one\r\n\r\nbody"), time.Now())
	e, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				err = e.SyncInbound(ctx, "")
			} else {
				err = e.SyncInboundKey(ctx, "inbound/one", time.Now())
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	restarted, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncInboundKey(ctx, "inbound/one", time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := restarted.Open(BoxINBOX)
	messages := b.Messages()
	if len(messages) != 1 || messages[0].UID != 1 {
		t.Fatalf("duplicate UID assignment: %+v", messages)
	}
}
func TestEventSkipsRemovedObjectAndSetupProbe(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, blob.NewMem(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"inbound/gone", "inbound/AMAZON_SES_SETUP_NOTIFICATION"} {
		if err := e.SyncInboundKey(ctx, key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := e.Open(BoxINBOX)
	if len(b.Messages()) != 0 {
		t.Fatal("imported missing object or setup probe")
	}
}
