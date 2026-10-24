package mailbox

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/denislemire/generic-mail/internal/state"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// StoreModified means successful conditional updates committed, but these IDs conflicted.
type StoreModified struct{ Set imap.NumSet }

func (e *StoreModified) Error() string { return "message state changed" }

// assignChangesLocked makes each actual change durable in the same transaction
// as its message state. No-op STOREs do not advance the mailbox clock.
func (e *Engine) assignChangesLocked(before map[*box]box) error {
	for _, b := range e.boxes {
		if b.highest == 0 {
			b.highest = 1
		}
		old := before[b]
		oldFlags := selectDataLocked(&old).Flags
		byUID := map[imap.UID]*message{}
		for _, m := range old.msgs {
			byUID[m.uid] = m
		}
		next := func() (uint64, error) {
			if b.highest >= uint64(1<<63-1) {
				return 0, fmt.Errorf("mailbox change number exhausted")
			}
			b.highest++
			return b.highest, nil
		}
		for _, m := range b.msgs {
			previous, ok := byUID[m.uid]
			if !ok || !reflect.DeepEqual(previous.flags, m.flags) {
				n, err := next()
				if err != nil {
					return err
				}
				m.modSeq = n
			}
			delete(byUID, m.uid)
		}
		// Walk old messages in UID order so the durable history is deterministic.
		for _, m := range old.msgs {
			if _, ok := byUID[m.uid]; ok {
				n, err := next()
				if err != nil {
					return err
				}
				b.expunged = append(b.expunged, state.Expunged{UID: uint32(m.uid), ModSeq: n})
			}
		}
		if flags := selectDataLocked(b).Flags; !reflect.DeepEqual(oldFlags, flags) {
			copyFlags := append([]imap.Flag(nil), flags...)
			e.pending = append(e.pending, func() { b.tracker.QueueMailboxFlags(copyFlags) })
		}
	}
	return nil
}

// SelectSync registers the session and snapshots its baseline under one lock.
func (b *Box) SelectSync(o *imap.SelectOptions) (*imap.SelectData, *imapserver.SessionTracker) {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	v := bx.tracker.NewSession()
	d := selectDataLocked(bx)
	if o != nil && o.QResync != nil && o.QResync.UIDValidity == bx.uidValid {
		q := o.QResync
		d.Vanished = vanishedLocked(bx, q.ModSeq, q.KnownUIDs)
		for i, m := range bx.msgs {
			if m.modSeq > q.ModSeq && (q.KnownUIDs == nil || q.KnownUIDs.Contains(m.uid)) {
				d.Changed = append(d.Changed, imap.SelectMessage{Seq: uint32(i + 1), UID: m.uid, Flags: flagList(m.flags), ModSeq: m.modSeq})
			}
		}
	}
	return d, v
}
func vanishedLocked(b *box, since uint64, known imap.UIDSet) imap.UIDSet {
	var out imap.UIDSet
	if since == 0 {
		// Before the initial modseq baseline we have no deletion journal. Report
		// absent allocated UIDs using compact ranges, without expanding UID space.
		var live []imap.UID
		for _, m := range b.msgs {
			live = append(live, m.uid)
		}
		sort.Slice(live, func(i, j int) bool { return live[i] < live[j] })
		next := imap.UID(1)
		add := func(start, end imap.UID) {
			if end < start {
				return
			}
			if known == nil {
				out.AddRange(start, end)
				return
			}
			for _, r := range known {
				lo, hi := r.Start, r.Stop
				if lo == 0 {
					lo = b.uidNext - 1
				}
				if hi == 0 {
					hi = b.uidNext - 1
				}
				if lo > hi {
					lo, hi = hi, lo
				}
				if lo < start {
					lo = start
				}
				if hi > end {
					hi = end
				}
				if lo <= hi {
					out.AddRange(lo, hi)
				}
			}
		}
		for _, uid := range live {
			if uid > next {
				add(next, uid-1)
			}
			next = uid + 1
		}
		if next < b.uidNext {
			add(next, b.uidNext-1)
		}
		return out
	}
	if known != nil && known.Dynamic() {
		known = append(imap.UIDSet(nil), known...)
		for i := range known {
			if known[i].Start == 0 {
				known[i].Start = b.uidNext - 1
			}
			if known[i].Stop == 0 {
				known[i].Stop = b.uidNext - 1
			}
		}
	}
	for _, x := range b.expunged {
		uid := imap.UID(x.UID)
		if x.ModSeq > since && (known == nil || known.Contains(uid)) {
			out.AddNum(uid)
		}
	}
	return out
}
func (b *Box) Vanished(since uint64, known imap.UIDSet) imap.UIDSet {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return vanishedLocked(b.box(), since, known)
}

// Keywords remain on the message through COPY and restart, and can be searched
// with KEYWORD/UNKEYWORD. Display names and colors can live above this layer.
func validateFlags(flags []imap.Flag) error {
	for _, flag := range flags {
		f := string(flag)
		if strings.EqualFold(f, `\Recent`) {
			continue
		}
		if strings.HasPrefix(f, `\`) {
			switch canonicalFlag(flag) {
			case canonicalFlag(imap.FlagSeen), canonicalFlag(imap.FlagAnswered), canonicalFlag(imap.FlagDeleted), canonicalFlag(imap.FlagDraft), canonicalFlag(imap.FlagFlagged):
				continue
			default:
				return errNo("unsupported system flag")
			}
		}
		if f == "" || len(f) > 255 {
			return errNo("invalid keyword")
		}
		for _, ch := range []byte(f) {
			if ch <= 32 || ch >= 127 || strings.ContainsRune(`(){%*"\]`, rune(ch)) {
				return errNo("invalid keyword")
			}
		}
	}
	return nil
}
