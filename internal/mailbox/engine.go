// Package mailbox is the interim Generic mailbox.
//
// SES already stores one raw RFC822 object per delivery under inbound/.
// This engine assigns each new key a UID in INBOX and keeps flags in a single
// JSON object (imap-state/mailboxes.json). UIDValidity is fixed for the life
// of that state object: if the object is deleted, clients must resync, which
// is the IMAP invariant Thunderbird depends on.
package mailbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

const (
	BoxINBOX  = "INBOX"
	BoxSent   = "Sent"
	BoxDrafts = "Drafts"
	BoxTrash  = "Trash"

	inboundPrefix = "inbound/"
	sentPrefix    = "sent/"
	draftsPrefix  = "drafts/"
)

// Engine is safe for concurrent IMAP sessions and one sync loop.
type Engine struct {
	blobs blob.Store

	mu          sync.Mutex
	commitMu    sync.Mutex
	boxes       map[string]*box
	skipped     map[string]struct{}
	etag        string
	ready       bool
	cache       map[string][]byte
	cacheN      int
	cacheBudget int
}

type box struct {
	name       string
	special    []imap.MailboxAttr
	uidValid   uint32
	uidNext    imap.UID
	subscribed bool
	msgs       []*message
	tracker    *imapserver.MailboxTracker
}

type message struct {
	uid      imap.UID
	key      string
	size     int64
	internal time.Time
	flags    map[imap.Flag]struct{}
	owned    bool
}

type fileState struct {
	Boxes   map[string]*fileBox `json:"boxes"`
	Skipped []string            `json:"skipped,omitempty"`
}

type fileBox struct {
	UIDValidity uint32    `json:"uidValidity"`
	UIDNext     uint32    `json:"uidNext"`
	Subscribed  bool      `json:"subscribed"`
	Messages    []fileMsg `json:"messages"`
}

type fileMsg struct {
	UID      uint32    `json:"uid"`
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Internal time.Time `json:"internal"`
	Flags    []string  `json:"flags,omitempty"`
	Owned    bool      `json:"owned,omitempty"`
}

// New loads state (if any) and ensures the four mailboxes exist.
func New(ctx context.Context, store blob.Store) (*Engine, error) {
	e := &Engine{
		blobs:       store,
		boxes:       map[string]*box{},
		skipped:     map[string]struct{}{},
		cache:       map[string][]byte{},
		cacheBudget: 64 << 20,
	}
	e.ensureBoxes()
	if err := e.reload(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// Ready is true after the first successful inbound sync.
func (e *Engine) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

func (e *Engine) ensureBoxes() {
	specs := []struct {
		name    string
		special []imap.MailboxAttr
	}{
		{BoxINBOX, nil},
		{BoxSent, []imap.MailboxAttr{imap.MailboxAttrSent}},
		{BoxDrafts, []imap.MailboxAttr{imap.MailboxAttrDrafts}},
		{BoxTrash, []imap.MailboxAttr{imap.MailboxAttrTrash}},
	}
	for _, spec := range specs {
		if _, ok := e.boxes[spec.name]; ok {
			continue
		}
		e.boxes[spec.name] = newBox(spec.name, spec.special)
	}
}

func newBox(name string, special []imap.MailboxAttr) *box {
	return &box{
		name:       name,
		special:    special,
		uidValid:   newValidity(),
		uidNext:    1,
		subscribed: true,
		tracker:    imapserver.NewMailboxTracker(0),
	}
}

func newValidity() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}
	v := binary.BigEndian.Uint32(b[:])
	if v == 0 {
		return 1
	}
	return v
}

// CanonBox accepts INBOX in any case and the three fixed folders.
func CanonBox(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "inbox":
		return BoxINBOX, true
	case "sent":
		return BoxSent, true
	case "drafts":
		return BoxDrafts, true
	case "trash":
		return BoxTrash, true
	default:
		return "", false
	}
}

// SyncInbound assigns UIDs to new objects under prefix (default inbound/).
func (e *Engine) SyncInbound(ctx context.Context, prefix string) error {
	if prefix == "" {
		prefix = inboundPrefix
	}
	metas, err := e.blobs.List(ctx, prefix)
	if err != nil {
		return err
	}
	e.mu.Lock()
	known := map[string]struct{}{}
	for _, m := range e.boxes[BoxINBOX].msgs {
		known[m.key] = struct{}{}
	}
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			known[m.key] = struct{}{}
		}
	}
	skipped := make(map[string]struct{}, len(e.skipped))
	for k := range e.skipped {
		skipped[k] = struct{}{}
	}
	e.mu.Unlock()

	type fresh struct {
		key      string
		raw      []byte
		internal time.Time
	}
	var added []fresh
	var skipMore []string
	for _, meta := range metas {
		if _, ok := known[meta.Key]; ok {
			continue
		}
		if _, ok := skipped[meta.Key]; ok {
			continue
		}
		raw, err := e.blobs.Get(ctx, meta.Key)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				continue
			}
			return err
		}
		if isSetupNotice(raw) {
			skipMore = append(skipMore, meta.Key)
			continue
		}
		when := meta.LastModified
		if when.IsZero() {
			when = time.Now().UTC()
		}
		if parsed := headerDate(raw); !parsed.IsZero() {
			when = parsed
		}
		added = append(added, fresh{key: meta.Key, raw: raw, internal: when.UTC()})
	}

	e.mu.Lock()
	for _, key := range skipMore {
		e.skipped[key] = struct{}{}
	}
	inbox := e.boxes[BoxINBOX]
	n := 0
	for _, item := range added {
		if e.hasKeyLocked(item.key) {
			continue
		}
		msg := &message{
			uid:      inbox.uidNext,
			key:      item.key,
			size:     int64(len(item.raw)),
			internal: item.internal,
			flags:    map[imap.Flag]struct{}{},
		}
		inbox.uidNext++
		inbox.msgs = append(inbox.msgs, msg)
		e.rememberLocked(item.key, item.raw)
		n++
	}
	if n > 0 || len(skipMore) > 0 {
		inbox.tracker.QueueNumMessages(uint32(len(inbox.msgs)))
	}
	e.ready = true
	e.mu.Unlock()

	if n > 0 || len(skipMore) > 0 {
		if err := e.commit(ctx); err != nil {
			return err
		}
	}
	if n > 0 {
		log.Printf("inbound sync: %d new message(s)", n)
	}
	return nil
}

func (e *Engine) hasKeyLocked(key string) bool {
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			if m.key == key {
				return true
			}
		}
	}
	return false
}

func isSetupNotice(raw []byte) bool {
	return bytes.Contains(bytes.ToLower(raw), []byte("amazon_ses_setup_notification"))
}

func headerDate(raw []byte) time.Time {
	br := bufio.NewReader(bytes.NewReader(raw))
	hdr, err := textproto.ReadHeader(br)
	if err != nil {
		return time.Time{}
	}
	h := mail.Header{Header: gomessage.Header{Header: hdr}}
	t, err := h.Date()
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *Engine) reload(ctx context.Context) error {
	body, etag, err := e.blobs.GetState(ctx, blob.StateKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var file fileState
	if err := json.Unmarshal(body, &file); err != nil {
		return fmt.Errorf("mailbox state: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.etag = etag
	e.skipped = map[string]struct{}{}
	for _, k := range file.Skipped {
		e.skipped[k] = struct{}{}
	}
	for name, fb := range file.Boxes {
		canon, ok := CanonBox(name)
		if !ok {
			continue
		}
		b := e.boxes[canon]
		b.uidValid = fb.UIDValidity
		if fb.UIDNext == 0 {
			b.uidNext = 1
		} else {
			b.uidNext = imap.UID(fb.UIDNext)
		}
		b.subscribed = fb.Subscribed
		b.msgs = nil
		for _, fm := range fb.Messages {
			flags := map[imap.Flag]struct{}{}
			for _, f := range fm.Flags {
				flags[canonicalFlag(imap.Flag(f))] = struct{}{}
			}
			b.msgs = append(b.msgs, &message{
				uid:      imap.UID(fm.UID),
				key:      fm.Key,
				size:     fm.Size,
				internal: fm.Internal,
				flags:    flags,
				owned:    fm.Owned,
			})
		}
		b.tracker = imapserver.NewMailboxTracker(uint32(len(b.msgs)))
	}
	e.ensureBoxes()
	return nil
}

func (e *Engine) commit(ctx context.Context) error {
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		e.mu.Lock()
		payload, err := json.Marshal(e.snapshotLocked())
		etag := e.etag
		e.mu.Unlock()
		if err != nil {
			return err
		}
		newETag, err := e.blobs.PutState(ctx, blob.StateKey, payload, etag)
		if err == nil {
			e.mu.Lock()
			e.etag = newETag
			e.mu.Unlock()
			return nil
		}
		if !errors.Is(err, blob.ErrPrecondition) {
			return err
		}
		_, cur, gerr := e.blobs.GetState(ctx, blob.StateKey)
		if gerr != nil && !errors.Is(gerr, blob.ErrNotFound) {
			return gerr
		}
		e.mu.Lock()
		e.etag = cur
		e.mu.Unlock()
	}
	return blob.ErrPrecondition
}

func (e *Engine) snapshotLocked() fileState {
	file := fileState{Boxes: map[string]*fileBox{}}
	for name, b := range e.boxes {
		fb := &fileBox{
			UIDValidity: b.uidValid,
			UIDNext:     uint32(b.uidNext),
			Subscribed:  b.subscribed,
		}
		for _, m := range b.msgs {
			fm := fileMsg{
				UID:      uint32(m.uid),
				Key:      m.key,
				Size:     m.size,
				Internal: m.internal,
				Owned:    m.owned,
			}
			for f := range m.flags {
				fm.Flags = append(fm.Flags, string(f))
			}
			fb.Messages = append(fb.Messages, fm)
		}
		file.Boxes[name] = fb
	}
	for k := range e.skipped {
		file.Skipped = append(file.Skipped, k)
	}
	return file
}

// Body returns the RFC822 bytes for an object key.
func (e *Engine) Body(ctx context.Context, key string) ([]byte, error) {
	e.mu.Lock()
	if raw, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return raw, nil
	}
	e.mu.Unlock()
	raw, err := e.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.rememberLocked(key, raw)
	e.mu.Unlock()
	return raw, nil
}

func (e *Engine) rememberLocked(key string, raw []byte) {
	budget := e.cacheBudget
	if budget <= 0 {
		budget = 64 << 20
	}
	if len(raw) > budget {
		return
	}
	if _, ok := e.cache[key]; ok {
		return
	}
	for e.cacheN+len(raw) > budget && len(e.cache) > 0 {
		for k, v := range e.cache {
			delete(e.cache, k)
			e.cacheN -= len(v)
			break
		}
	}
	if e.cacheN+len(raw) > budget {
		return
	}
	e.cache[key] = raw
	e.cacheN += len(raw)
}

// Box is one mailbox handle used by an authenticated session.
type Box struct {
	e    *Engine
	name string
}

// Commit writes the UID and flag state back to the blob store.
func (e *Engine) Commit(ctx context.Context) error { return e.commit(ctx) }

// Open returns a mailbox by name.
func (e *Engine) Open(name string) (*Box, error) {
	canon, ok := CanonBox(name)
	if !ok {
		return nil, errNo("no such mailbox")
	}
	return &Box{e: e, name: canon}, nil
}

func (b *Box) Name() string { return b.name }

func (b *Box) box() *box { return b.e.boxes[b.name] }

// ListData is the LIST metadata for every mailbox.
func (e *Engine) ListData(subscribedOnly, specialOnly, wantSpecial bool) []imap.ListData {
	e.mu.Lock()
	defer e.mu.Unlock()
	order := []string{BoxINBOX, BoxSent, BoxDrafts, BoxTrash}
	var out []imap.ListData
	for _, name := range order {
		bx := e.boxes[name]
		if subscribedOnly && !bx.subscribed {
			continue
		}
		if specialOnly && len(bx.special) == 0 {
			continue
		}
		data := imap.ListData{Mailbox: name, Delim: '/'}
		if bx.subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		data.Attrs = append(data.Attrs, imap.MailboxAttrHasNoChildren)
		if wantSpecial || specialOnly {
			data.Attrs = append(data.Attrs, bx.special...)
		}
		out = append(out, data)
	}
	return out
}

// SelectData is the SELECT/EXAMINE payload.
func (b *Box) SelectData() *imap.SelectData {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	return &imap.SelectData{
		Flags:          []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft},
		PermanentFlags: []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft, imap.FlagWildcard},
		NumMessages:    uint32(len(bx.msgs)),
		UIDNext:        bx.uidNext,
		UIDValidity:    bx.uidValid,
	}
}

// Tracker is the mailbox's unilateral-update tracker.
func (b *Box) Tracker() *imapserver.MailboxTracker {
	return b.box().tracker
}

// StatusData implements STATUS.
func (b *Box) StatusData(opts *imap.StatusOptions) *imap.StatusData {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	data := &imap.StatusData{Mailbox: bx.name}
	if opts != nil && opts.NumMessages {
		n := uint32(len(bx.msgs))
		data.NumMessages = &n
	}
	if opts != nil && opts.UIDNext {
		data.UIDNext = bx.uidNext
	}
	if opts != nil && opts.UIDValidity {
		data.UIDValidity = bx.uidValid
	}
	if opts != nil && opts.NumUnseen {
		var n uint32
		for _, m := range bx.msgs {
			if _, ok := m.flags[canonicalFlag(imap.FlagSeen)]; !ok {
				n++
			}
		}
		data.NumUnseen = &n
	}
	if opts != nil && opts.NumDeleted {
		var n uint32
		seen := canonicalFlag(imap.FlagDeleted)
		for _, m := range bx.msgs {
			if _, ok := m.flags[seen]; ok {
				n++
			}
		}
		data.NumDeleted = &n
	}
	if opts != nil && opts.Size {
		var n int64
		for _, m := range bx.msgs {
			n += m.size
		}
		data.Size = &n
	}
	if opts != nil && opts.NumRecent {
		z := uint32(0)
		data.NumRecent = &z
	}
	return data
}

// Append stores a message in Sent or Drafts. INBOX is the SES receipt set.
func (b *Box) Append(ctx context.Context, r io.Reader, opts *imap.AppendOptions) (*imap.AppendData, error) {
	if b.name != BoxSent && b.name != BoxDrafts {
		return nil, errNo("APPEND is only supported for Sent and Drafts")
	}
	raw, err := io.ReadAll(io.LimitReader(r, 30<<20+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 30<<20 {
		return nil, errNo("message exceeds 30MiB")
	}
	prefix := sentPrefix
	if b.name == BoxDrafts {
		prefix = draftsPrefix
	}
	key := prefix + randomID()
	if err := b.e.blobs.Put(ctx, key, raw); err != nil {
		return nil, err
	}
	when := time.Now().UTC()
	var flags []imap.Flag
	if opts != nil {
		if !opts.Time.IsZero() {
			when = opts.Time.UTC()
		}
		flags = opts.Flags
	}
	b.e.mu.Lock()
	bx := b.box()
	msg := &message{
		uid:      bx.uidNext,
		key:      key,
		size:     int64(len(raw)),
		internal: when,
		flags:    map[imap.Flag]struct{}{},
		owned:    true,
	}
	for _, f := range flags {
		msg.flags[canonicalFlag(f)] = struct{}{}
	}
	bx.uidNext++
	bx.msgs = append(bx.msgs, msg)
	uid := msg.uid
	valid := bx.uidValid
	bx.tracker.QueueNumMessages(uint32(len(bx.msgs)))
	b.e.rememberLocked(key, raw)
	b.e.mu.Unlock()
	if err := b.e.commit(ctx); err != nil {
		return nil, err
	}
	return &imap.AppendData{UID: uid, UIDValidity: valid}, nil
}

// Msg is a fetched message handle.
type Msg struct {
	Seq      uint32
	UID      imap.UID
	Key      string
	Size     int64
	Internal time.Time
	Flags    []imap.Flag
}

// ForEach calls fn for each selected message. numSet is resolved under the lock.
func (b *Box) ForEach(numSet imap.NumSet, fn func(Msg)) {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	numSet = staticNumSet(numSet, bx)
	for i, m := range bx.msgs {
		seq := uint32(i + 1)
		if !contains(numSet, seq, m.uid) {
			continue
		}
		fn(Msg{
			Seq:      seq,
			UID:      m.uid,
			Key:      m.key,
			Size:     m.size,
			Internal: m.internal,
			Flags:    flagList(m.flags),
		})
	}
}

// SetSeen marks messages that were fetched without PEEK.
func (b *Box) SetSeen(uids []imap.UID) {
	if len(uids) == 0 {
		return
	}
	want := map[imap.UID]struct{}{}
	for _, u := range uids {
		want[u] = struct{}{}
	}
	b.e.mu.Lock()
	bx := b.box()
	seen := canonicalFlag(imap.FlagSeen)
	for i, m := range bx.msgs {
		if _, ok := want[m.uid]; !ok {
			continue
		}
		if _, ok := m.flags[seen]; ok {
			continue
		}
		m.flags[seen] = struct{}{}
		bx.tracker.QueueMessageFlags(uint32(i+1), m.uid, flagList(m.flags), nil)
	}
	b.e.mu.Unlock()
}

// Store updates flags. It returns the messages that changed, for a non-silent FETCH.
func (b *Box) Store(numSet imap.NumSet, op imap.StoreFlagsOp, flags []imap.Flag, session *imapserver.SessionTracker) []Msg {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	numSet = staticNumSet(numSet, bx)
	var changed []Msg
	for i, m := range bx.msgs {
		seq := uint32(i + 1)
		if !contains(numSet, seq, m.uid) {
			continue
		}
		applyFlags(m.flags, op, flags)
		list := flagList(m.flags)
		bx.tracker.QueueMessageFlags(seq, m.uid, list, session)
		changed = append(changed, Msg{Seq: seq, UID: m.uid, Key: m.key, Size: m.size, Internal: m.internal, Flags: list})
	}
	return changed
}

// Expunge removes \Deleted messages from this mailbox.
// Inbound SES objects are never deleted. Owned Sent/Drafts objects are deleted
// once nothing else references the key.
func (b *Box) Expunge(ctx context.Context, uids *imap.UIDSet) error {
	b.e.mu.Lock()
	bx := b.box()
	del := canonicalFlag(imap.FlagDeleted)
	var keep []*message
	var drop []*message
	for i := len(bx.msgs) - 1; i >= 0; i-- {
		m := bx.msgs[i]
		_, flagged := m.flags[del]
		if flagged && (uids == nil || uids.Contains(m.uid)) {
			drop = append(drop, m)
			bx.tracker.QueueExpunge(uint32(i + 1))
		}
	}
	if len(drop) == 0 {
		b.e.mu.Unlock()
		return nil
	}
	dropSet := map[*message]struct{}{}
	for _, m := range drop {
		dropSet[m] = struct{}{}
	}
	for _, m := range bx.msgs {
		if _, ok := dropSet[m]; !ok {
			keep = append(keep, m)
		}
	}
	bx.msgs = keep
	var keys []string
	for _, m := range drop {
		if m.owned && !b.e.hasKeyLocked(m.key) {
			keys = append(keys, m.key)
		}
	}
	b.e.mu.Unlock()
	if err := b.e.commit(ctx); err != nil {
		return err
	}
	for _, key := range keys {
		if err := b.e.blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
			log.Printf("delete %s: %v", key, err)
		}
	}
	return nil
}

// Copy duplicates index entries into dest. Bodies are not rewritten.
func (b *Box) Copy(ctx context.Context, numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	destCanon, ok := CanonBox(destName)
	if !ok {
		return nil, errNo("no such mailbox")
	}
	if destCanon == BoxINBOX {
		return nil, errNo("INBOX only accepts mail delivered by SES")
	}
	b.e.mu.Lock()
	src := b.box()
	dst := b.e.boxes[destCanon]
	numSet = staticNumSet(numSet, src)
	var srcUIDs, dstUIDs imap.UIDSet
	for i, m := range src.msgs {
		seq := uint32(i + 1)
		if !contains(numSet, seq, m.uid) {
			continue
		}
		cp := &message{
			uid:      dst.uidNext,
			key:      m.key,
			size:     m.size,
			internal: m.internal,
			flags:    copyFlags(m.flags),
			owned:    false,
		}
		delete(cp.flags, canonicalFlag(imap.FlagDeleted))
		dst.uidNext++
		dst.msgs = append(dst.msgs, cp)
		srcUIDs.AddNum(m.uid)
		dstUIDs.AddNum(cp.uid)
	}
	valid := dst.uidValid
	if srcUIDs.String() != "" {
		dst.tracker.QueueNumMessages(uint32(len(dst.msgs)))
	}
	b.e.mu.Unlock()
	if srcUIDs.String() == "" {
		return &imap.CopyData{UIDValidity: valid}, nil
	}
	if err := b.e.commit(ctx); err != nil {
		return nil, err
	}
	return &imap.CopyData{UIDValidity: valid, SourceUIDs: srcUIDs, DestUIDs: dstUIDs}, nil
}

// Messages returns every message currently in the mailbox.
func (b *Box) Messages() []Msg {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	out := make([]Msg, 0, len(bx.msgs))
	for i, m := range bx.msgs {
		out = append(out, Msg{
			Seq:      uint32(i + 1),
			UID:      m.uid,
			Key:      m.key,
			Size:     m.size,
			Internal: m.internal,
			Flags:    flagList(m.flags),
		})
	}
	return out
}

// SetSubscribed records an IMAP subscription.
func (b *Box) SetSubscribed(ctx context.Context, on bool) error {
	b.e.mu.Lock()
	b.box().subscribed = on
	b.e.mu.Unlock()
	return b.e.commit(ctx)
}

func (b *Box) MatchNum(numSet imap.NumSet) []Msg {
	var out []Msg
	b.ForEach(numSet, func(m Msg) { out = append(out, m) })
	return out
}

func applyFlags(dst map[imap.Flag]struct{}, op imap.StoreFlagsOp, flags []imap.Flag) {
	switch op {
	case imap.StoreFlagsSet:
		for k := range dst {
			delete(dst, k)
		}
		fallthrough
	case imap.StoreFlagsAdd:
		for _, f := range flags {
			dst[canonicalFlag(f)] = struct{}{}
		}
	case imap.StoreFlagsDel:
		for _, f := range flags {
			delete(dst, canonicalFlag(f))
		}
	}
}

func copyFlags(in map[imap.Flag]struct{}) map[imap.Flag]struct{} {
	out := make(map[imap.Flag]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func flagList(flags map[imap.Flag]struct{}) []imap.Flag {
	out := make([]imap.Flag, 0, len(flags))
	for f := range flags {
		out = append(out, f)
	}
	return out
}

func canonicalFlag(flag imap.Flag) imap.Flag {
	return imap.Flag(strings.ToLower(string(flag)))
}

func contains(numSet imap.NumSet, seq uint32, uid imap.UID) bool {
	switch s := numSet.(type) {
	case imap.SeqSet:
		return s.Contains(seq)
	case imap.UIDSet:
		return s.Contains(uid)
	default:
		return false
	}
}

func staticNumSet(numSet imap.NumSet, bx *box) imap.NumSet {
	switch s := numSet.(type) {
	case imap.SeqSet:
		max := uint32(len(bx.msgs))
		for i := range s {
			staticRange(&s[i].Start, &s[i].Stop, max)
		}
		return s
	case imap.UIDSet:
		max := uint32(0)
		if bx.uidNext > 1 {
			max = uint32(bx.uidNext - 1)
		}
		for i := range s {
			start := uint32(s[i].Start)
			stop := uint32(s[i].Stop)
			staticRange(&start, &stop, max)
			s[i].Start = imap.UID(start)
			s[i].Stop = imap.UID(stop)
		}
		return s
	default:
		return numSet
	}
}

func staticRange(start, stop *uint32, max uint32) {
	dyn := false
	if *start == 0 {
		*start = max
		dyn = true
	}
	if *stop == 0 {
		*stop = max
		dyn = true
	}
	if dyn && *start > *stop {
		*start, *stop = *stop, *start
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

func errNo(text string) error {
	err := imap.Error(imap.StatusResponse{
		Type: imap.StatusResponseTypeNo,
		Text: text,
	})
	return &err
}
