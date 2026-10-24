// Package mailbox is the interim Generic mailbox.
//
// SES inbound objects stay immutable in S3. UIDs, flags, and folders live in a
// local SQLite file. Copying a message into a folder writes a local file and
// points the index at that file. UIDValidity is fixed for the life of the
// database: if the volume is lost, clients must resync.
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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/state"

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
)

// Engine is safe for concurrent IMAP sessions and one sync loop.
type Engine struct {
	blobs blob.Store

	mu          sync.Mutex
	db          *state.DB
	files       string
	boxes       map[string]*box
	seen        map[string]struct{}
	ready       bool
	cache       map[string][]byte
	cacheN      int
	cacheBudget int
	pending     []func()
}

const (
	locS3   = "s3"
	locFile = "file"
)

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
	loc      string
	key      string
	source   string
	size     int64
	internal time.Time
	flags    map[imap.Flag]struct{}
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

// New opens the local index under dataDir and ensures the four mailboxes exist.
// An empty database imports imap-state/mailboxes.json once, if that object exists,
// and does not write it back.
func New(ctx context.Context, store blob.Store, dataDir string) (*Engine, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "files"), 0o750); err != nil {
		return nil, err
	}
	db, err := state.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		blobs:       store,
		db:          db,
		files:       filepath.Join(dataDir, "files"),
		boxes:       map[string]*box{},
		seen:        map[string]struct{}{},
		cache:       map[string][]byte{},
		cacheBudget: 64 << 20,
	}
	e.ensureBoxes()
	empty, err := db.Empty()
	if err != nil {
		return nil, err
	}
	if empty {
		if err := e.importLegacy(ctx); err != nil {
			return nil, err
		}
		if err := e.commit(ctx); err != nil {
			return nil, err
		}
		return e, nil
	}
	if err := e.loadDB(); err != nil {
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
	for k := range e.seen {
		known[k] = struct{}{}
	}
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			if m.loc == locS3 {
				known[m.key] = struct{}{}
			}
			if m.source != "" {
				known[m.source] = struct{}{}
			}
		}
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
	n := 0
	err = e.mutateLocked(func() error {
		for _, key := range skipMore {
			e.seen[key] = struct{}{}
		}
		inbox := e.boxes[BoxINBOX]
		for _, item := range added {
			if _, ok := e.seen[item.key]; ok {
				continue
			}
			msg := &message{
				uid:      inbox.uidNext,
				loc:      locS3,
				key:      item.key,
				source:   item.key,
				size:     int64(len(item.raw)),
				internal: item.internal,
				flags:    map[imap.Flag]struct{}{},
			}
			inbox.uidNext++
			inbox.msgs = append(inbox.msgs, msg)
			e.seen[item.key] = struct{}{}
			e.rememberLocked(locS3, item.key, item.raw)
			n++
		}
		if n > 0 || len(skipMore) > 0 {
			e.queueCount(inbox)
		}
		e.ready = true
		return nil
	})
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("inbound sync: %d new message(s)", n)
	}
	return nil
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

func (e *Engine) importLegacy(ctx context.Context) error {
	body, _, err := e.blobs.GetState(ctx, blob.StateKey)
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
	for _, k := range file.Skipped {
		e.seen[k] = struct{}{}
	}
	for name, fb := range file.Boxes {
		canon, ok := CanonBox(name)
		if !ok || fb == nil {
			continue
		}
		b := e.boxes[canon]
		if fb.UIDValidity != 0 {
			b.uidValid = fb.UIDValidity
		}
		b.subscribed = fb.Subscribed
		b.msgs = nil
		var maxUID imap.UID
		for _, fm := range fb.Messages {
			flags := map[imap.Flag]struct{}{}
			for _, f := range fm.Flags {
				flags[canonicalFlag(imap.Flag(f))] = struct{}{}
			}
			if fm.Key != "" {
				e.seen[fm.Key] = struct{}{}
			}
			msg := &message{
				uid:      imap.UID(fm.UID),
				loc:      locS3,
				key:      fm.Key,
				source:   fm.Key,
				size:     fm.Size,
				internal: fm.Internal,
				flags:    flags,
			}
			if msg.uid > maxUID {
				maxUID = msg.uid
			}
			b.msgs = append(b.msgs, msg)
		}
		sort.Slice(b.msgs, func(i, j int) bool { return b.msgs[i].uid < b.msgs[j].uid })
		next := imap.UID(fb.UIDNext)
		if next <= maxUID {
			next = maxUID + 1
		}
		if next == 0 {
			next = 1
		}
		b.uidNext = next
		b.tracker = imapserver.NewMailboxTracker(uint32(len(b.msgs)))
	}
	e.ensureBoxes()
	return nil
}

func (e *Engine) loadDB() error {
	snap, err := e.db.Load()
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, key := range snap.Seen {
		e.seen[key] = struct{}{}
	}
	for _, fb := range snap.Boxes {
		b := e.boxes[fb.Name]
		if b == nil {
			b = newBox(fb.Name, nil)
			e.boxes[fb.Name] = b
		}
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
			if fm.Source != "" {
				e.seen[fm.Source] = struct{}{}
			}
			if fm.Loc == locS3 {
				e.seen[fm.Ref] = struct{}{}
			}
			b.msgs = append(b.msgs, &message{
				uid:      imap.UID(fm.UID),
				loc:      fm.Loc,
				key:      fm.Ref,
				source:   fm.Source,
				size:     fm.Size,
				internal: fm.Internal,
				flags:    flags,
			})
		}
		b.tracker = imapserver.NewMailboxTracker(uint32(len(b.msgs)))
	}
	e.ensureBoxes()
	return nil
}

func (e *Engine) commit(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.Save(e.snapshotLocked())
}

// mutateLocked keeps failed writes invisible and publishes tracker updates only
// after SQLite commits. The caller holds mu throughout mutation and persistence.
func (e *Engine) mutateLocked(change func() error) error {
	original := make(map[string]*box, len(e.boxes))
	saved := make(map[*box]box, len(e.boxes))
	for name, b := range e.boxes {
		original[name] = b
		copyBox := *b
		copyBox.msgs = make([]*message, len(b.msgs))
		for i, m := range b.msgs {
			copyMsg := *m
			copyMsg.flags = copyFlags(m.flags)
			copyBox.msgs[i] = &copyMsg
		}
		saved[b] = copyBox
	}
	seen := make(map[string]struct{}, len(e.seen))
	for key := range e.seen {
		seen[key] = struct{}{}
	}
	ready := e.ready
	e.pending = nil
	err := change()
	if err == nil {
		err = e.db.Save(e.snapshotLocked())
	}
	if err != nil {
		for b, copyBox := range saved {
			*b = copyBox
		}
		e.boxes, e.seen, e.ready = original, seen, ready
	} else {
		for _, notify := range e.pending {
			notify()
		}
	}
	e.pending = nil
	return err
}

func (e *Engine) queueCount(b *box) {
	n := uint32(len(b.msgs))
	e.pending = append(e.pending, func() { b.tracker.QueueNumMessages(n) })
}

func (e *Engine) queueFlags(b *box, seq uint32, m *message, source *imapserver.SessionTracker) {
	uid, flags := m.uid, flagList(m.flags)
	e.pending = append(e.pending, func() { b.tracker.QueueMessageFlags(seq, uid, flags, source) })
}

func (e *Engine) queueExpunge(b *box, seq uint32) {
	e.pending = append(e.pending, func() { b.tracker.QueueExpunge(seq) })
}

func (e *Engine) snapshotLocked() state.Snap {
	names := make([]string, 0, len(e.boxes))
	for name := range e.boxes {
		names = append(names, name)
	}
	sort.Strings(names)
	var snap state.Snap
	for _, name := range names {
		b := e.boxes[name]
		box := state.Box{
			Name:        name,
			UIDValidity: b.uidValid,
			UIDNext:     uint32(b.uidNext),
			Subscribed:  b.subscribed,
		}
		for _, m := range b.msgs {
			fm := state.Msg{
				UID:      uint32(m.uid),
				Loc:      m.loc,
				Ref:      m.key,
				Source:   m.source,
				Size:     m.size,
				Internal: m.internal,
			}
			for f := range m.flags {
				fm.Flags = append(fm.Flags, string(f))
			}
			sort.Strings(fm.Flags)
			box.Messages = append(box.Messages, fm)
		}
		snap.Boxes = append(snap.Boxes, box)
	}
	for key := range e.seen {
		snap.Seen = append(snap.Seen, key)
	}
	sort.Strings(snap.Seen)
	return snap
}

// Body returns the RFC822 bytes for an index location.
func (e *Engine) Body(ctx context.Context, loc, key string) ([]byte, error) {
	ck := loc + "\n" + key
	e.mu.Lock()
	if raw, ok := e.cache[ck]; ok {
		e.mu.Unlock()
		return append([]byte(nil), raw...), nil
	}
	e.mu.Unlock()
	var raw []byte
	var err error
	if loc == locFile {
		raw, err = os.ReadFile(filepath.Join(e.files, key))
	} else {
		raw, err = e.blobs.Get(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.rememberLocked(loc, key, raw)
	e.mu.Unlock()
	return raw, nil
}

func (e *Engine) rememberLocked(loc, key string, raw []byte) {
	key = loc + "\n" + key
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
	e      *Engine
	target *box
}

// Commit persists the current UID and flag state to SQLite.
func (e *Engine) Commit(ctx context.Context) error { return e.commit(ctx) }

// Open returns a mailbox by name.
func (e *Engine) Open(name string) (*Box, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	canon, ok := e.resolveLocked(name)
	if !ok {
		return nil, errNo("no such mailbox")
	}
	return &Box{e: e, target: e.boxes[canon]}, nil
}

func (e *Engine) resolveLocked(name string) (string, bool) {
	if canon, ok := CanonBox(name); ok {
		if _, exists := e.boxes[canon]; exists {
			return canon, true
		}
	}
	if _, ok := e.boxes[name]; ok {
		return name, true
	}
	return "", false
}

func (b *Box) Name() string {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return b.target.name
}

// Exists reports whether the mailbox still exists, including after a rename.
func (b *Box) Exists() bool {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return b.e.boxes[b.target.name] == b.target
}

func (b *Box) box() *box { return b.target }

// ListData is the LIST metadata for every mailbox.
func (e *Engine) ListData(subscribedOnly, specialOnly, wantSpecial bool) []imap.ListData {
	e.mu.Lock()
	defer e.mu.Unlock()
	order := []string{BoxINBOX, BoxSent, BoxDrafts, BoxTrash}
	var extra []string
	for name := range e.boxes {
		if _, ok := CanonBox(name); !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)
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
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
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

// Append stores a message as a local file. INBOX is the SES receipt set.
func (b *Box) Append(ctx context.Context, r io.Reader, opts *imap.AppendOptions) (*imap.AppendData, error) {
	if b.Name() == BoxINBOX {
		return nil, errNo("INBOX only accepts mail delivered by SES")
	}
	raw, err := io.ReadAll(io.LimitReader(r, 30<<20+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 30<<20 {
		return nil, errNo("message exceeds 30MiB")
	}
	id, err := b.e.writeFile(raw)
	if err != nil {
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
	defer b.e.mu.Unlock()
	var data *imap.AppendData
	err = b.e.mutateLocked(func() error {
		bx := b.box()
		if b.e.boxes[bx.name] != bx {
			return errNo("no such mailbox")
		}
		msg := &message{uid: bx.uidNext, loc: locFile, key: id, size: int64(len(raw)), internal: when, flags: map[imap.Flag]struct{}{}}
		for _, f := range flags {
			msg.flags[canonicalFlag(f)] = struct{}{}
		}
		bx.uidNext++
		bx.msgs = append(bx.msgs, msg)
		b.e.queueCount(bx)
		data = &imap.AppendData{UID: msg.uid, UIDValidity: bx.uidValid}
		return nil
	})
	if err != nil {
		_ = os.Remove(filepath.Join(b.e.files, id))
		return nil, err
	}
	b.e.rememberLocked(locFile, id, raw)
	return data, nil
}

func (e *Engine) writeFile(raw []byte) (string, error) {
	id := randomID()
	tmp := filepath.Join(e.files, id+".tmp")
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(e.files, id)); err != nil {
		return "", err
	}
	return id, nil
}

// Msg is a fetched message handle.
type Msg struct {
	Seq      uint32
	UID      imap.UID
	Loc      string
	Key      string
	Size     int64
	Internal time.Time
	Flags    []imap.Flag
}

// ForEach calls fn for each selected message. The mailbox lock is released
// before fn runs, so fn may call Body without deadlocking.
func (b *Box) ForEach(numSet imap.NumSet, fn func(Msg), views ...*imapserver.SessionTracker) {
	view := firstView(views)
	b.e.mu.Lock()
	bx := b.box()
	numSet = staticNumSetView(numSet, bx, view)
	matched := make([]Msg, 0)
	for i, m := range bx.msgs {
		seq := uint32(i + 1)
		if !containsView(numSet, seq, m.uid, view) {
			continue
		}
		if view != nil {
			seq = view.EncodeSeqNum(seq)
		}
		if seq == 0 {
			continue
		}
		matched = append(matched, Msg{
			Seq:      seq,
			UID:      m.uid,
			Loc:      m.loc,
			Key:      m.key,
			Size:     m.size,
			Internal: m.internal,
			Flags:    flagList(m.flags),
		})
	}
	b.e.mu.Unlock()
	for _, msg := range matched {
		fn(msg)
	}
}

// SetSeen marks messages that were fetched without PEEK.
func (b *Box) SetSeen(uids []imap.UID) error {
	if len(uids) == 0 {
		return nil
	}
	want := map[imap.UID]struct{}{}
	for _, uid := range uids {
		want[uid] = struct{}{}
	}
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	return b.e.mutateLocked(func() error {
		bx := b.box()
		if b.e.boxes[bx.name] != bx {
			return errNo("no such mailbox")
		}
		seen := canonicalFlag(imap.FlagSeen)
		for i, m := range bx.msgs {
			if _, ok := want[m.uid]; !ok {
				continue
			}
			if _, ok := m.flags[seen]; ok {
				continue
			}
			m.flags[seen] = struct{}{}
			b.e.queueFlags(bx, uint32(i+1), m, nil)
		}
		return nil
	})
}

// Store commits flags before returning them or notifying other sessions.
func (b *Box) Store(numSet imap.NumSet, op imap.StoreFlagsOp, flags []imap.Flag, session *imapserver.SessionTracker) ([]Msg, error) {
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	var changed []Msg
	err := b.e.mutateLocked(func() error {
		bx := b.box()
		if b.e.boxes[bx.name] != bx {
			return errNo("no such mailbox")
		}
		numSet = staticNumSetView(numSet, bx, session)
		for i, m := range bx.msgs {
			seq := uint32(i + 1)
			if !containsView(numSet, seq, m.uid, session) {
				continue
			}
			applyFlags(m.flags, op, flags)
			b.e.queueFlags(bx, seq, m, session)
			if session != nil {
				seq = session.EncodeSeqNum(seq)
			}
			changed = append(changed, Msg{Seq: seq, UID: m.uid, Loc: m.loc, Key: m.key, Size: m.size, Internal: m.internal, Flags: flagList(m.flags)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// Expunge removes \Deleted messages from this mailbox.
// S3 objects are never deleted. A local file is removed when no row still points at it.
func (b *Box) Expunge(ctx context.Context, uids *imap.UIDSet) error {
	b.e.mu.Lock()
	var files []string
	err := b.e.mutateLocked(func() error {
		bx := b.box()
		if b.e.boxes[bx.name] != bx {
			return errNo("no such mailbox")
		}
		del := canonicalFlag(imap.FlagDeleted)
		var keep []*message
		var drop []*message
		for i := len(bx.msgs) - 1; i >= 0; i-- {
			m := bx.msgs[i]
			_, flagged := m.flags[del]
			if flagged && (uids == nil || uids.Contains(m.uid)) {
				drop = append(drop, m)
				b.e.queueExpunge(bx, uint32(i+1))
			}
		}
		if len(drop) == 0 {
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
		for _, m := range drop {
			if m.loc == locS3 {
				b.e.seen[m.key] = struct{}{}
			}
			if m.source != "" {
				b.e.seen[m.source] = struct{}{}
			}
		}
		for _, m := range drop {
			if m.loc == locFile && !b.e.fileUsedLocked(m.key) {
				files = append(files, m.key)
			}
		}
		return nil
	})
	b.e.mu.Unlock()
	if err != nil {
		return err
	}
	for _, id := range files {
		if err := os.Remove(filepath.Join(b.e.files, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("delete local %s: %v", id, err)
		}
	}
	return nil
}

func (e *Engine) fileUsedLocked(id string) bool {
	for _, b := range e.boxes {
		for _, m := range b.msgs {
			if m.loc == locFile && m.key == id {
				return true
			}
		}
	}
	return false
}

// Copy writes a local file for each message and points the destination at it.
// The source object, including inbound S3, is left unchanged.
func (b *Box) Copy(ctx context.Context, numSet imap.NumSet, destName string, views ...*imapserver.SessionTracker) (*imap.CopyData, error) {
	view := firstView(views)
	b.e.mu.Lock()
	dest, ok := b.e.resolveLocked(destName)
	if !ok {
		b.e.mu.Unlock()
		return nil, errNo("no such mailbox")
	}
	if dest == BoxINBOX {
		b.e.mu.Unlock()
		return nil, errNo("INBOX only accepts mail delivered by SES")
	}
	destination := b.e.boxes[dest]
	src := b.box()
	if b.e.boxes[src.name] != src {
		b.e.mu.Unlock()
		return nil, errNo("no such mailbox")
	}
	numSet = staticNumSetView(numSet, src, view)
	type item struct {
		loc, key, source string
		internal         time.Time
		flags            map[imap.Flag]struct{}
		uid              imap.UID
	}
	var todo []item
	var srcUIDs imap.UIDSet
	for i, m := range src.msgs {
		seq := uint32(i + 1)
		if !containsView(numSet, seq, m.uid, view) {
			continue
		}
		source := m.source
		if source == "" && m.loc == locS3 {
			source = m.key
		}
		todo = append(todo, item{
			loc: m.loc, key: m.key, source: source, internal: m.internal,
			flags: copyFlags(m.flags), uid: m.uid,
		})
		srcUIDs.AddNum(m.uid)
	}
	b.e.mu.Unlock()
	if len(todo) == 0 {
		b.e.mu.Lock()
		valid := destination.uidValid
		b.e.mu.Unlock()
		return &imap.CopyData{UIDValidity: valid}, nil
	}
	type written struct {
		item
		id   string
		size int64
	}
	var wrote []written
	committed := false
	defer func() {
		if !committed {
			for _, w := range wrote {
				_ = os.Remove(filepath.Join(b.e.files, w.id))
			}
		}
	}()
	for _, it := range todo {
		raw, err := b.e.Body(ctx, it.loc, it.key)
		if err != nil {
			return nil, err
		}
		id, err := b.e.writeFile(raw)
		if err != nil {
			return nil, err
		}
		wrote = append(wrote, written{item: it, id: id, size: int64(len(raw))})
	}
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	dst := destination
	var dstUIDs imap.UIDSet
	err := b.e.mutateLocked(func() error {
		if b.e.boxes[dst.name] != dst {
			return errNo("no such mailbox")
		}
		for _, w := range wrote {
			flags := w.flags
			delete(flags, canonicalFlag(imap.FlagDeleted))
			msg := &message{
				uid:      dst.uidNext,
				loc:      locFile,
				key:      w.id,
				source:   w.source,
				size:     w.size,
				internal: w.internal,
				flags:    flags,
			}
			dst.uidNext++
			dst.msgs = append(dst.msgs, msg)
			if msg.source != "" {
				b.e.seen[msg.source] = struct{}{}
			}
			dstUIDs.AddNum(msg.uid)
		}
		b.e.queueCount(dst)
		return nil
	})
	if err != nil {
		return nil, err
	}
	committed = true
	return &imap.CopyData{UIDValidity: dst.uidValid, SourceUIDs: srcUIDs, DestUIDs: dstUIDs}, nil
}

// Create adds a mailbox. The four standard names already exist.
func (e *Engine) Create(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if !validFolder(name) {
		return errNo("invalid mailbox name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutateLocked(func() error {
		if _, ok := e.resolveLocked(name); ok {
			return errNo("mailbox already exists")
		}
		e.boxes[name] = newBox(name, nil)
		return nil
	})
}

// Delete removes an empty mailbox that is not one of the four standard names.
func (e *Engine) Delete(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutateLocked(func() error {
		canon, ok := e.resolveLocked(name)
		if !ok {
			return errNo("no such mailbox")
		}
		if _, special := CanonBox(canon); special {
			return errNo("mailbox is required")
		}
		if len(e.boxes[canon].msgs) != 0 {
			return errNo("mailbox is not empty")
		}
		delete(e.boxes, canon)
		return nil
	})
}

// Rename renames a mailbox that is not one of the four standard names.
func (e *Engine) Rename(ctx context.Context, from, to string) error {
	to = strings.TrimSpace(to)
	if !validFolder(to) {
		return errNo("invalid mailbox name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutateLocked(func() error {
		canon, ok := e.resolveLocked(from)
		if !ok {
			return errNo("no such mailbox")
		}
		if _, special := CanonBox(canon); special {
			return errNo("mailbox is required")
		}
		if _, exists := e.resolveLocked(to); exists {
			return errNo("mailbox already exists")
		}
		e.boxes[to] = e.boxes[canon]
		e.boxes[to].name = to
		delete(e.boxes, canon)
		return nil
	})
}

func validFolder(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	if strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
		return false
	}
	switch strings.ToLower(name) {
	case "inbox", "sent", "drafts", "trash":
		return false
	}
	return true
}

// Messages returns every message currently in the mailbox.
func (b *Box) Messages(views ...*imapserver.SessionTracker) []Msg {
	view := firstView(views)
	b.e.mu.Lock()
	defer b.e.mu.Unlock()
	bx := b.box()
	out := make([]Msg, 0, len(bx.msgs))
	for i, m := range bx.msgs {
		seq := uint32(i + 1)
		if view != nil {
			seq = view.EncodeSeqNum(seq)
		}
		if seq == 0 {
			continue
		}
		out = append(out, Msg{
			Seq:      seq,
			UID:      m.uid,
			Loc:      m.loc,
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
	defer b.e.mu.Unlock()
	return b.e.mutateLocked(func() error {
		bx := b.box()
		if b.e.boxes[bx.name] != bx {
			return errNo("no such mailbox")
		}
		bx.subscribed = on
		return nil
	})
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
		switch canonicalFlag(f) {
		case canonicalFlag(imap.FlagSeen):
			f = imap.FlagSeen
		case canonicalFlag(imap.FlagAnswered):
			f = imap.FlagAnswered
		case canonicalFlag(imap.FlagFlagged):
			f = imap.FlagFlagged
		case canonicalFlag(imap.FlagDeleted):
			f = imap.FlagDeleted
		case canonicalFlag(imap.FlagDraft):
			f = imap.FlagDraft
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
	return staticNumSetView(numSet, bx, nil)
}

func firstView(views []*imapserver.SessionTracker) *imapserver.SessionTracker {
	if len(views) > 0 {
		return views[0]
	}
	return nil
}

func containsView(set imap.NumSet, seq uint32, uid imap.UID, view *imapserver.SessionTracker) bool {
	if _, ok := set.(imap.SeqSet); ok && view != nil {
		seq = view.EncodeSeqNum(seq)
		if seq == 0 {
			return false
		}
	}
	return contains(set, seq, uid)
}

func staticNumSetView(numSet imap.NumSet, bx *box, view *imapserver.SessionTracker) imap.NumSet {
	switch s := numSet.(type) {
	case imap.SeqSet:
		s = append(imap.SeqSet(nil), s...)
		max := uint32(len(bx.msgs))
		if view != nil {
			for max > 0 && view.EncodeSeqNum(max) == 0 {
				max--
			}
			max = view.EncodeSeqNum(max)
		}
		for i := range s {
			staticRange(&s[i].Start, &s[i].Stop, max)
		}
		return s
	case imap.UIDSet:
		s = append(imap.UIDSet(nil), s...)
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
