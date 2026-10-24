package imapd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/denislemire/generic-mail/internal/applepush"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/denislemire/generic-mail/internal/oauth"
	"github.com/denislemire/generic-mail/internal/state"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
)

// Server serves one mailbox over IMAP.
type Server struct {
	Engine    *mailbox.Engine
	Username  string
	Password  string
	OAuth     *oauth.Checker
	PushTopic string
}

func (s *Server) NewSession() imapserver.Session {
	return &session{srv: s}
}

type session struct {
	srv           *Server
	authed        bool
	box           *mailbox.Box
	view          *imapserver.SessionTracker
	readOnly      bool
	expungeModSeq uint64
}

func (s *session) Close() error {
	s.dropView()
	return nil
}

func (s *session) dropView() {
	if s.view != nil {
		s.view.Close()
		s.view = nil
	}
	s.box = nil
}

func (s *session) Login(username, password string) error {
	userOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(username)), []byte(strings.ToLower(s.srv.Username))) == 1
	passOK := len(password) == len(s.srv.Password) && subtle.ConstantTimeCompare([]byte(password), []byte(s.srv.Password)) == 1
	if !userOK || !passOK {
		return imapserver.ErrAuthFailed
	}
	s.authed = true
	return nil
}

func (s *session) AuthenticateMechanisms() []string {
	mechs := []string{sasl.Plain}
	if s.srv.OAuth.Enabled() {
		mechs = append(mechs, oauth.XOAuth2, sasl.OAuthBearer)
	}
	return mechs
}

func (s *session) Authenticate(mech string) (sasl.Server, error) {
	switch strings.ToUpper(mech) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && !strings.EqualFold(identity, username) {
				return imapserver.ErrAuthFailed
			}
			return s.Login(username, password)
		}), nil
	case oauth.XOAuth2, sasl.OAuthBearer:
		if !s.srv.OAuth.Enabled() {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SASL mechanism not supported"}
		}
		return oauth.NewServer(func(username, token string) error {
			if err := s.srv.OAuth.Accept(username, token); err != nil {
				return imapserver.ErrAuthFailed
			}
			s.authed = true
			return nil
		}), nil
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SASL mechanism not supported"}
	}
}

func (s *session) needAuth() error {
	if !s.authed {
		return no("authenticate first")
	}
	return nil
}

func (s *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	box, err := s.srv.Engine.Open(name)
	if err != nil {
		return nil, err
	}
	s.dropView()
	s.box = box
	data, view := box.SelectSync(options)
	s.view = view
	s.readOnly = options != nil && options.ReadOnly
	return data, nil
}

func (s *session) Unselect() error {
	s.dropView()
	s.readOnly = false
	return nil
}

func (s *session) Create(name string, _ *imap.CreateOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	return s.srv.Engine.Create(context.Background(), name)
}

func (s *session) Delete(name string) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	return s.srv.Engine.Delete(context.Background(), name)
}

func (s *session) Rename(from, to string, _ *imap.RenameOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	return s.srv.Engine.Rename(context.Background(), from, to)
}

func (s *session) Subscribe(name string) error   { return s.setSub(name, true) }
func (s *session) Unsubscribe(name string) error { return s.setSub(name, false) }

func (s *session) setSub(name string, on bool) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	box, err := s.srv.Engine.Open(name)
	if err != nil {
		return err
	}
	return box.SetSubscribed(context.Background(), on)
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	if options == nil {
		options = &imap.ListOptions{}
	}
	for _, data := range s.srv.Engine.ListData(options.SelectSubscribed, options.SelectSpecialUse, options.ReturnSpecialUse) {
		if !matchList(data.Mailbox, ref, patterns) {
			continue
		}
		item := data
		if err := w.WriteList(&item); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	box, err := s.srv.Engine.Open(name)
	if err != nil {
		return nil, err
	}
	return box.StatusData(options), nil
}

func (s *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if err := s.needAuth(); err != nil {
		return nil, err
	}
	box, err := s.srv.Engine.Open(name)
	if err != nil {
		return nil, err
	}
	return box.Append(context.Background(), r, options)
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.view == nil {
		return nil
	}
	return s.view.Poll(w, allowExpunge)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.view == nil {
		return no("select a mailbox before IDLE")
	}
	return s.view.Idle(w, stop)
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if err := s.selected(); err != nil {
		return err
	}
	if s.readOnly {
		return no("mailbox is read-only")
	}
	_ = w
	return s.box.Expunge(context.Background(), uids, func(n uint64) { s.expungeModSeq = n })
}

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if err := s.selected(); err != nil {
		return nil, err
	}
	_ = options
	messages := s.box.Messages(s.view)
	var candidates map[state.ContentKey]bool
	if query := mailbox.SearchIndexQuery(criteria); query != "" {
		// Derived-index failure falls back to exact local scanning.
		if found, err := s.srv.Engine.SearchCandidates(context.Background(), query); err == nil {
			candidates = found
		}
	}
	var data imap.SearchData
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	for _, msg := range messages {
		if msg.Seq == 0 {
			continue
		}
		matched, known := mailbox.MatchMetadata(msg, msg.Seq, criteria)
		if known && !matched {
			continue
		}
		if !known {
			if candidates != nil && !candidates[state.ContentKey{Loc: msg.Loc, Ref: msg.Key}] {
				continue
			}
			raw, err := s.srv.Engine.Body(context.Background(), msg.Loc, msg.Key)
			if err != nil {
				return nil, err
			}
			if !mailbox.Match(msg, raw, msg.Seq, criteria) {
				continue
			}
		}
		uidSet.AddNum(msg.UID)
		var num uint32
		switch kind {
		case imapserver.NumKindSeq:
			seqSet.AddNum(msg.Seq)
			num = msg.Seq
		default:
			num = uint32(msg.UID)
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if data.Max == 0 || num > data.Max {
			data.Max = num
		}
		data.Count++
		if hasModSeq(criteria) && msg.ModSeq > data.ModSeq {
			data.ModSeq = msg.ModSeq
		}
	}
	switch kind {
	case imapserver.NumKindSeq:
		data.All = seqSet
	default:
		data.All = uidSet
	}
	return &data, nil
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if err := s.selected(); err != nil {
		return err
	}
	if options == nil {
		options = &imap.FetchOptions{}
	}
	markSeen := false
	for _, section := range options.BodySection {
		if !section.Peek {
			markSeen = true
			break
		}
	}

	if options.Vanished {
		if uids, ok := numSet.(imap.UIDSet); ok {
			gone := s.box.Vanished(options.ChangedSince, uids)
			if len(gone) > 0 {
				if err := w.WriteVanished(gone); err != nil {
					return err
				}
			}
		}
	}
	var fetchErr error
	s.box.ForEach(numSet, func(msg mailbox.Msg) {
		if fetchErr != nil {
			return
		}
		if (options.ChangedSinceSet || options.ChangedSince > 0) && msg.ModSeq <= options.ChangedSince {
			return
		}
		seq := msg.Seq
		if seq == 0 {
			return
		}
		var raw []byte
		if fetchNeedsBody(options) {
			var err error
			raw, err = s.srv.Engine.Body(context.Background(), msg.Loc, msg.Key)
			if err != nil {
				fetchErr = err
				return
			}
		}
		if markSeen && !s.readOnly {
			if err := s.box.SetSeen([]imap.UID{msg.UID}); err != nil {
				fetchErr = err
				return
			}
			for _, updated := range s.box.Messages(s.view) {
				if updated.UID == msg.UID {
					msg = updated
					break
				}
			}
			found := false
			for _, flag := range msg.Flags {
				found = found || strings.EqualFold(string(flag), string(imap.FlagSeen))
			}
			if !found {
				msg.Flags = append(msg.Flags, imap.FlagSeen)
			}
		}
		mw := w.CreateMessage(seq)
		responseOptions := *options
		responseOptions.Flags = options.Flags || (markSeen && !s.readOnly)
		responseOptions.ModSeq = options.ModSeq || (w.CondStore() && responseOptions.Flags)
		fetchErr = writeFetch(mw, msg, raw, &responseOptions)
	}, s.view)
	return fetchErr
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if err := s.selected(); err != nil {
		return err
	}
	if s.readOnly {
		return no("mailbox is read-only")
	}

	changed, err := s.box.Store(numSet, flags.Op, flags.Flags, s.view, options)
	var modified *mailbox.StoreModified
	if err != nil && !errors.As(err, &modified) {
		return err
	}
	if flags.Silent && !w.CondStore() {
		return err
	}

	for _, msg := range changed {
		seq := msg.Seq
		if seq == 0 {
			continue
		}
		mw := w.CreateMessage(seq)
		if err := writeFetch(mw, msg, nil, &imap.FetchOptions{Flags: !flags.Silent, ModSeq: w.CondStore()}); err != nil {
			return err
		}
	}
	if modified != nil {
		return &imap.Error{Type: imap.StatusResponseTypeOK, Code: imap.ResponseCode("MODIFIED " + modified.Set.String()), Text: "Conditional STORE completed"}
	}
	return nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	if err := s.selected(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, no("mailbox is read-only")
	}
	return s.box.Copy(context.Background(), numSet, dest, s.view)
}

func (s *session) selected() error {
	if err := s.needAuth(); err != nil {
		return err
	}
	if s.box == nil || !s.box.Exists() {
		return no("select a mailbox")
	}
	return nil
}

func matchList(name, ref string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	full := name
	if ref != "" && !strings.EqualFold(ref, "INBOX") {
		full = strings.TrimRight(ref, "/") + "/" + name
	}
	for _, pattern := range patterns {
		if pattern == "*" || pattern == "%" || strings.EqualFold(pattern, full) || strings.EqualFold(pattern, name) {
			return true
		}
	}
	return false
}

func no(text string) error {
	err := imap.Error(imap.StatusResponse{Type: imap.StatusResponseTypeNo, Text: text})
	return &err
}

func writeFetch(w *imapserver.FetchResponseWriter, msg mailbox.Msg, raw []byte, options *imap.FetchOptions) error {
	w.WriteUID(msg.UID)
	if options.ModSeq {
		w.WriteModSeq(msg.ModSeq)
	}
	if options.Flags {
		w.WriteFlags(msg.Flags)
	}
	if options.InternalDate {
		w.WriteInternalDate(msg.Internal)
	}
	if options.RFC822Size {
		w.WriteRFC822Size(msg.Size)
	}
	if options.Envelope {
		br := bufio.NewReader(bytes.NewReader(raw))
		hdr, err := textproto.ReadHeader(br)
		if err != nil {
			w.WriteEnvelope(&imap.Envelope{})
		} else {
			w.WriteEnvelope(imapserver.ExtractEnvelope(hdr))
		}
	}
	if options.BodyStructure != nil {
		w.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	for _, section := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(raw), section)
		wc := w.WriteBodySection(section, int64(len(buf)))
		_, writeErr := wc.Write(buf)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return w.Close()
}

// Metadata operations must not depend on S3 or local body availability.
func fetchNeedsBody(options *imap.FetchOptions) bool {
	return options.Envelope || options.BodyStructure != nil || len(options.BodySection) > 0
}

// ApplePushTopic is empty until Mail-specific APNs credentials are configured.
func (s *session) ApplePushTopic() string {
	if !s.authed {
		return ""
	}
	return s.srv.PushTopic
}
func (s *session) RegisterApplePush(registration *imapserver.ApplePushRegistration) error {
	if err := s.needAuth(); err != nil {
		return err
	}
	if s.srv.PushTopic == "" {
		return no("Apple push is not enabled")
	}
	if registration == nil {
		return no("invalid Apple push registration")
	}
	token, names, err := applepush.NormalizeRegistration(registration.AccountID, registration.DeviceToken, registration.Subtopic, registration.Mailboxes)
	if err != nil {
		return no("invalid Apple push registration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.srv.Engine.RegisterPush(ctx, registration.AccountID, token, names)
}

func hasModSeq(c *imap.SearchCriteria) bool {
	if c.ModSeq != nil {
		return true
	}
	for i := range c.Not {
		if hasModSeq(&c.Not[i]) {
			return true
		}
	}
	for i := range c.Or {
		if hasModSeq(&c.Or[i][0]) || hasModSeq(&c.Or[i][1]) {
			return true
		}
	}
	return false
}
func (s *session) HighestModSeq() uint64 {
	if s.box == nil {
		return 0
	}
	n := s.box.SelectData().HighestModSeq
	if limit := s.PendingExpungeLimit(); limit != 0 && limit < n {
		return limit
	}
	return n
}

func (s *session) PendingExpungeLimit() uint64 {
	if s.view == nil {
		return 0
	}
	return s.view.PendingExpungeLimit()
}

func (s *session) ExpungeModSeq() uint64 { return s.expungeModSeq }
