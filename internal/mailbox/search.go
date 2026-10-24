package mailbox

import (
	"bytes"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

func Match(msg Msg, raw []byte, seq uint32, criteria *imap.SearchCriteria) bool {
	return matchCriteria(msg, raw, seq, criteria)
}

func matchCriteria(msg Msg, raw []byte, seq uint32, criteria *imap.SearchCriteria) bool {
	if criteria == nil {
		return true
	}
	for _, seqSet := range criteria.SeqNum {
		if seq == 0 || !seqSet.Contains(seq) {
			return false
		}
	}
	for _, uidSet := range criteria.UID {
		if !uidSet.Contains(msg.UID) {
			return false
		}
	}
	if !matchDate(msg.Internal, criteria.Since, criteria.Before) {
		return false
	}
	flags := map[imap.Flag]struct{}{}
	for _, f := range msg.Flags {
		flags[canonicalFlag(f)] = struct{}{}
	}
	for _, f := range criteria.Flag {
		if _, ok := flags[canonicalFlag(f)]; !ok {
			return false
		}
	}
	for _, f := range criteria.NotFlag {
		if _, ok := flags[canonicalFlag(f)]; ok {
			return false
		}
	}
	if criteria.Larger != 0 && msg.Size <= criteria.Larger {
		return false
	}
	if criteria.Smaller != 0 && msg.Size >= criteria.Smaller {
		return false
	}
	lower := bytes.ToLower(raw)
	for _, h := range criteria.Header {
		if !headerContains(raw, h.Key, h.Value) {
			return false
		}
	}
	body := bodyOnly(raw)
	for _, part := range criteria.Body {
		if !bytes.Contains(bytes.ToLower(body), bytes.ToLower([]byte(part))) {
			return false
		}
	}
	for _, part := range criteria.Text {
		if !bytes.Contains(lower, bytes.ToLower([]byte(part))) {
			return false
		}
	}
	if !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
		sent := headerDate(raw)
		if sent.IsZero() || !matchDate(sent, criteria.SentSince, criteria.SentBefore) {
			return false
		}
	}
	for _, not := range criteria.Not {
		if matchCriteria(msg, raw, seq, &not) {
			return false
		}
	}
	for _, or := range criteria.Or {
		if !matchCriteria(msg, raw, seq, &or[0]) && !matchCriteria(msg, raw, seq, &or[1]) {
			return false
		}
	}
	return true
}

func matchDate(t, since, before time.Time) bool {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

func headerContains(raw []byte, key, value string) bool {
	key = strings.ToLower(key)
	value = strings.ToLower(value)
	for _, line := range bytes.Split(headerBlock(raw), []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 || (line[0] == ' ' || line[0] == '\t') {
			continue
		}
		name, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok || strings.ToLower(string(name)) != key {
			continue
		}
		if value == "" || strings.Contains(strings.ToLower(string(rest)), value) {
			return true
		}
	}
	return false
}

func headerBlock(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i]
	}
	return raw
}

func bodyOnly(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[i+2:]
	}
	return raw
}

// MatchMetadata evaluates the boolean expression without content I/O. An
// unknown result means exact matching needs MIME bytes. In particular OR with
// an already true metadata arm and NOT of a known arm remain I/O-free.
func MatchMetadata(msg Msg, seq uint32, c *imap.SearchCriteria) (matched, known bool) {
	if c == nil {
		return true, true
	}
	metadata := *c
	metadata.Header = nil
	metadata.Body = nil
	metadata.Text = nil
	metadata.SentSince = time.Time{}
	metadata.SentBefore = time.Time{}
	metadata.Not = nil
	metadata.Or = nil
	if !Match(msg, nil, seq, &metadata) {
		return false, true
	}
	known = len(c.Header) == 0 && len(c.Body) == 0 && len(c.Text) == 0 && c.SentSince.IsZero() && c.SentBefore.IsZero()
	for _, child := range c.Not {
		m, k := MatchMetadata(msg, seq, &child)
		if k && m {
			return false, true
		}
		known = known && k
	}
	for _, arms := range c.Or {
		a, ak := MatchMetadata(msg, seq, &arms[0])
		b, bk := MatchMetadata(msg, seq, &arms[1])
		if (ak && a) || (bk && b) {
			continue
		}
		if ak && bk {
			return false, true
		}
		known = false
	}
	return true, known
}

// SearchIndexQuery builds only necessary positive substring conditions. NOT
// and unindexable terms remain unrestricted. OR can narrow candidates only if
// both branches can be indexed. Exact IMAP matching always follows the index.
func SearchIndexQuery(c *imap.SearchCriteria) string {
	if c == nil {
		return ""
	}
	var parts []string
	term := func(v string) string {
		if len(v) < 3 {
			return ""
		}
		for _, r := range v {
			if r < 32 || r > 126 {
				return ""
			}
		}
		return `"` + strings.ReplaceAll(strings.ToLower(v), `"`, `""`) + `"`
	}
	for _, v := range c.Body {
		if q := term(v); q != "" {
			parts = append(parts, q)
		}
	}
	for _, v := range c.Text {
		if q := term(v); q != "" {
			parts = append(parts, q)
		}
	}
	for _, h := range c.Header {
		if q := term(h.Value); q != "" {
			parts = append(parts, q)
		}
	}
	for _, arms := range c.Or {
		a, b := SearchIndexQuery(&arms[0]), SearchIndexQuery(&arms[1])
		if a != "" && b != "" {
			parts = append(parts, "("+a+" OR "+b+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}
