package mailbox

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/state"
	"github.com/emersion/go-imap/v2"
)

func TestSyncHistoryAtomicAndPersistent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := blob.NewMem()
	e, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.Open(BoxDrafts)
	if _, err = b.Append(ctx, strings.NewReader("Subject: synthetic\r\n\r\nbody"), nil); err != nil {
		t.Fatal(err)
	}
	baseline := b.SelectData()
	before := e.snapshotLocked()
	e.db.Close()
	if _, err = b.Store(imap.UIDSetNum(1), imap.StoreFlagsAdd, []imap.Flag{"Finance"}, nil); err == nil {
		t.Fatal("expected failure")
	}
	if !reflect.DeepEqual(before, e.snapshotLocked()) {
		t.Fatal("failed STORE leaked flags or modseq")
	}
	e.db, err = state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Store(imap.UIDSetNum(1), imap.StoreFlagsAdd, []imap.Flag{imap.FlagDeleted}, nil); err != nil {
		t.Fatal(err)
	}
	before = e.snapshotLocked()
	e.db.Close()
	if err = b.Expunge(ctx, nil); err == nil {
		t.Fatal("expected failed expunge")
	}
	if !reflect.DeepEqual(before, e.snapshotLocked()) {
		t.Fatal("failed expunge leaked tombstone or clock")
	}
	e.db, err = state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Expunge(ctx, nil); err != nil {
		t.Fatal(err)
	}
	current := b.SelectData()
	if current.HighestModSeq <= baseline.HighestModSeq {
		t.Fatal("deletion did not advance clock")
	}
	e.db.Close()
	e, err = New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.db.Close()
	b, _ = e.Open(BoxDrafts)
	if b.SelectData().HighestModSeq != current.HighestModSeq {
		t.Fatal("restart lost clock")
	}
	if !b.Vanished(baseline.HighestModSeq, nil).Contains(1) {
		t.Fatal("restart lost expunge")
	}
}
func TestConcurrentConditionalStoreHasOneWinner(t *testing.T) {
	e, err := New(context.Background(), blob.NewMem(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.db.Close()
	b, _ := e.Open(BoxDrafts)
	b.Append(context.Background(), strings.NewReader("Subject: synthetic\r\n\r\nbody"), nil)
	h := b.SelectData().HighestModSeq
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, flag := range []imap.Flag{"finance", "business"} {
		wg.Add(1)
		go func(f imap.Flag) {
			defer wg.Done()
			<-start
			_, err := b.Store(imap.UIDSetNum(1), imap.StoreFlagsAdd, []imap.Flag{f}, nil, &imap.StoreOptions{UnchangedSince: h})
			errs <- err
		}(flag)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		var conflict *StoreModified
		if err == nil {
			wins++
		} else if errors.As(err, &conflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}
func TestKeywordCopyPersistenceAndBaselineUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Save(state.Snap{Boxes: []state.Box{{Name: "Drafts", UIDValidity: 42, UIDNext: 1}}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store := blob.NewMem()
	e, err := New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.Open(BoxDrafts)
	if b.SelectData().HighestModSeq == 0 || b.SelectData().UIDValidity != 42 {
		t.Fatal("bad legacy upgrade")
	}
	a, err := b.Append(ctx, strings.NewReader("Subject: synthetic\r\n\r\nbody"), &imap.AppendOptions{Flags: []imap.Flag{"Finance", "Business"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Create(ctx, "Labels-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Copy(ctx, imap.UIDSetNum(a.UID), "Labels-test"); err != nil {
		t.Fatal(err)
	}
	e.db.Close()
	e, err = New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.db.Close()
	copy, _ := e.Open("Labels-test")
	m := copy.Messages()[0]
	if m.ModSeq == 0 || !Match(m, nil, 1, &imap.SearchCriteria{Flag: []imap.Flag{"FINANCE", "business"}}) {
		t.Fatal("keyword/modseq lost on COPY or restart")
	}
	if _, err = b.Store(imap.UIDSetNum(1), imap.StoreFlagsAdd, []imap.Flag{`\*`}, nil); err == nil {
		t.Fatal("wildcard should not be a stored keyword")
	}
}
