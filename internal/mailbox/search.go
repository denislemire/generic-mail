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
