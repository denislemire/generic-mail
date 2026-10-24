package mailbox

import (
	"context"
	"fmt"
	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/state"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestMetadataBooleanDecisionsAgreeWithExactMatch(t *testing.T) {
	raw := []byte("Subject: invoice\r\n\r\nhello world")
	atoms := []imap.SearchCriteria{{}, {Flag: []imap.Flag{imap.FlagSeen}}, {NotFlag: []imap.Flag{imap.FlagSeen}}, {Body: []string{"world"}}, {Body: []string{"absent"}}, {UID: []imap.UIDSet{imap.UIDSetNum(2)}}, {Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "invoice"}}}, {Since: time.Now().AddDate(0, 0, 1)}}
	for _, flags := range [][]imap.Flag{nil, {imap.FlagSeen}} {
		msg := Msg{UID: 1, Internal: time.Now(), Flags: flags}
		for _, a := range atoms {
			for _, b := range atoms {
				for _, c := range []imap.SearchCriteria{a, {Not: []imap.SearchCriteria{a}}, {Or: [][2]imap.SearchCriteria{{a, b}}}, {Not: []imap.SearchCriteria{{Or: [][2]imap.SearchCriteria{{a, b}}}}}} {
					got, known := MatchMetadata(msg, 1, &c)
					if known && got != Match(msg, raw, 1, &c) {
						t.Fatalf("unsound metadata decision: %+v", c)
					}
				}
			}
		}
	}
}
func TestSearchQueryConservativeBooleanCases(t *testing.T) {
	for _, c := range []*imap.SearchCriteria{
		nil, {}, {Body: []string{"ab"}}, {Text: []string{"naïve"}}, {Not: []imap.SearchCriteria{{Text: []string{"invoice"}}}},
		{Or: [][2]imap.SearchCriteria{{{Text: []string{"invoice"}}, {Flag: []imap.Flag{imap.FlagSeen}}}}},
	} {
		if q := SearchIndexQuery(c); q != "" {
			t.Fatalf("unsafe candidate restriction %s", q)
		}
	}
	if q := SearchIndexQuery(&imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{{Text: []string{"invoice"}}, {Text: []string{"billing"}}}}}); q == "" {
		t.Fatal("missing indexable OR")
	}
}

func BenchmarkSearchIndex(b *testing.B) {
	ctx := context.Background()
	e, err := New(ctx, blob.NewMem(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	raws := map[state.ContentKey][]byte{}
	snap := state.Snap{Boxes: []state.Box{{Name: "INBOX", UIDValidity: 1, UIDNext: 701}}}
	for i := 0; i < 700; i++ {
		key := fmt.Sprintf("inbound/%04d", i)
		raw := []byte("Subject: benchmark\r\n\r\n" + strings.Repeat("ordinary mailbox content ", 400))
		if i%100 == 0 {
			raw = append(raw, []byte(" unique-needle-value ")...)
		}
		k := state.ContentKey{Loc: locS3, Ref: key}
		raws[k] = raw
		snap.Boxes[0].Messages = append(snap.Boxes[0].Messages, state.Msg{UID: uint32(i + 1), Loc: k.Loc, Ref: k.Ref})
	}
	if err = e.db.Save(snap); err != nil {
		b.Fatal(err)
	}
	for k, raw := range raws {
		if err = e.db.IndexContent(ctx, k, raw); err != nil {
			b.Fatal(err)
		}
	}
	criteria := &imap.SearchCriteria{Text: []string{"unique-needle-value"}}
	query := SearchIndexQuery(criteria)
	b.Run("scan", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			count := 0
			for _, raw := range raws {
				if Match(Msg{}, raw, 1, criteria) {
					count++
				}
			}
			if count != 7 {
				b.Fatal(count)
			}
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			keys, err := e.db.SearchCandidates(ctx, query)
			if err != nil {
				b.Fatal(err)
			}
			count := 0
			for k := range keys {
				if Match(Msg{}, raws[k], 1, criteria) {
					count++
				}
			}
			if count != 7 {
				b.Fatal(count)
			}
		}
	})
}
