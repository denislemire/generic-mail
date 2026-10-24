package state

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func searchSnap(keys ...string) Snap {
	s := Snap{Boxes: []Box{{Name: "INBOX", UIDValidity: 1, UIDNext: uint32(len(keys) + 1)}}}
	for i, key := range keys {
		s.Boxes[0].Messages = append(s.Boxes[0].Messages, Msg{UID: uint32(i + 1), Loc: "s3", Ref: key})
	}
	return s
}
func TestSearchCandidatesAndGarbageCollection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Save(searchSnap("yes", "no", "unindexed", "huge", "binary")); err != nil {
		t.Fatal(err)
	}
	for key, raw := range map[string][]byte{"yes": []byte("Subject: InvoICe\r\n\r\nhello"), "no": []byte("other message"), "huge": bytes.Repeat([]byte("x"), maxIndexedBytes+1), "binary": []byte("before\x00\xff Invoice after")} {
		if err = d.IndexContent(ctx, ContentKey{Loc: "s3", Ref: key}, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	candidates, err := d.SearchCandidates(ctx, `"invoice"`)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"yes", "binary", "huge", "unindexed"} {
		if !candidates[ContentKey{Loc: "s3", Ref: key}] {
			t.Fatalf("lost candidate %s", key)
		}
	}
	if candidates[ContentKey{Loc: "s3", Ref: "no"}] {
		t.Fatal("index did not filter negative document")
	}
	if err = d.Save(searchSnap()); err != nil {
		t.Fatal(err)
	}
	var docs, fts int
	if err = d.sql.QueryRow(`SELECT count(*) FROM search_document`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if err = d.sql.QueryRow(`SELECT count(*) FROM search_fts`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if docs != 0 || fts != 0 {
		t.Fatal("orphan search projections survived expunge")
	}
	if err = d.IndexContent(ctx, ContentKey{Loc: "s3", Ref: "yes"}, []byte("invoice")); err != nil {
		t.Fatal(err)
	}
	indexed, err := d.ContentIndexed(ctx, ContentKey{Loc: "s3", Ref: "yes"})
	if err != nil || indexed {
		t.Fatal("in-flight indexing resurrected removed document")
	}
}
