package imapd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/denislemire/generic-mail/internal/mailbox"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type wireClient struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
	n int
}

func syncWire(t *testing.T, e *mailbox.Engine) *wireClient {
	t.Helper()
	b := &Server{Engine: e, Username: "owner", Password: "password"}
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapQResync: {}, imap.CapUIDPlus: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return b.NewSession(), nil, nil
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(20 * time.Second))
	w := &wireClient{t: t, c: c, r: bufio.NewReader(c)}
	t.Cleanup(func() { c.Close(); srv.Close(); <-done })
	if _, err = w.r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	w.ok("LOGIN owner password")
	return w
}
func (w *wireClient) cmd(s string) string {
	w.t.Helper()
	w.n++
	tag := fmt.Sprintf("t%d", w.n)
	if _, err := fmt.Fprintf(w.c, "%s %s\r\n", tag, s); err != nil {
		w.t.Fatal(err)
	}
	var out strings.Builder
	for {
		line, err := w.r.ReadString('\n')
		if err != nil {
			w.t.Fatalf("%s: %v; output=%s", s, err, out.String())
		}
		out.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return out.String()
		}
	}
}
func (w *wireClient) ok(s string) string {
	w.t.Helper()
	out := w.cmd(s)
	if !strings.Contains(out, fmt.Sprintf("t%d OK", w.n)) {
		w.t.Fatalf("%s: %s", s, out)
	}
	return out
}
func number(t *testing.T, s, key string) uint64 {
	t.Helper()
	m := regexp.MustCompile(key + `[ (]+([0-9]+)`).FindStringSubmatch(s)
	if len(m) != 2 {
		t.Fatalf("missing %s: %s", key, s)
	}
	n, _ := strconv.ParseUint(m[1], 10, 64)
	return n
}
func has(t *testing.T, s, part string) {
	t.Helper()
	if !strings.Contains(s, part) {
		t.Fatalf("missing %q: %s", part, s)
	}
}
func lacks(t *testing.T, s, part string) {
	t.Helper()
	if strings.Contains(s, part) {
		t.Fatalf("unexpected %q: %s", part, s)
	}
}
func seeded(t *testing.T) (*mailbox.Engine, blob.Store, string) {
	t.Helper()
	store := blob.NewMem()
	dir := t.TempDir()
	e, err := mailbox.New(context.Background(), store, dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := e.Open("Drafts")
	for i := 0; i < 3; i++ {
		if _, err := box.Append(context.Background(), strings.NewReader("Subject: synthetic\r\n\r\nbody\r\n"), nil); err != nil {
			t.Fatal(err)
		}
	}
	return e, store, dir
}

func TestCondStoreConditionalUpdatesAndKeywords(t *testing.T) {
	e, _, _ := seeded(t)
	a, b := syncWire(t, e), syncWire(t, e)
	has(t, a.ok("CAPABILITY"), "CONDSTORE QRESYNC")
	sel := a.ok("SELECT Drafts (CONDSTORE)")
	h := number(t, sel, "HIGHESTMODSEQ")
	if h < 4 {
		t.Fatal(h)
	}
	b.ok("SELECT Drafts")
	// First activating command in a selected mailbox must supply the baseline.
	has(t, b.ok("UID FETCH 1 (MODSEQ)"), "HIGHESTMODSEQ")
	out := a.ok("UID STORE 1 +FLAGS.SILENT (Finance)")
	has(t, out, "MODSEQ")
	lacks(t, out, "UID 1 FLAGS (")
	next := number(t, out, "MODSEQ")
	if next <= h {
		t.Fatal(out)
	}
	out = a.ok("UID STORE 1 +FLAGS.SILENT (FINANCE)")
	if number(t, out, "MODSEQ") != next {
		t.Fatal("no-op advanced clock", out)
	}
	out = b.ok(fmt.Sprintf("UID STORE 1:2 (UNCHANGEDSINCE %d) +FLAGS.SILENT (Business)", h))
	has(t, out, "[MODIFIED 1]")
	has(t, out, "UID 2 MODSEQ")
	has(t, out, "UID 1 FLAGS (finance) MODSEQ")
	out = a.ok(fmt.Sprintf("UID FETCH 1:3 (FLAGS) (CHANGEDSINCE %d)", h))
	has(t, out, "UID 1")
	has(t, out, "UID 2")
	lacks(t, out, "UID 3")
	has(t, a.ok("UID SEARCH KEYWORD FINANCE"), "* SEARCH 1")
	has(t, a.ok("UID SEARCH UNKEYWORD Finance"), "* SEARCH 2 3")
	has(t, a.ok(fmt.Sprintf("UID SEARCH MODSEQ %d", next)), "(MODSEQ ")
	out = a.ok("UID STORE 1:3 (UNCHANGEDSINCE 0) +FLAGS (Nope)")
	has(t, out, "[MODIFIED 1:3]")
	out = a.ok("STATUS Drafts (HIGHESTMODSEQ)")
	has(t, out, "HIGHESTMODSEQ")
	out = a.ok("SELECT Drafts")
	has(t, out, "finance")
	has(t, out, "business")
	has(t, out, `\*`)
}
func TestQResyncReconnectSurvivesRestart(t *testing.T) {
	e, store, dir := seeded(t)
	a := syncWire(t, e)
	a.ok("ENABLE QRESYNC")
	sel := a.ok("SELECT Drafts")
	h := number(t, sel, "HIGHESTMODSEQ")
	valid := number(t, sel, "UIDVALIDITY")
	a.ok("UID STORE 1 +FLAGS (Finance)")
	a.ok(`UID STORE 2 +FLAGS (\Deleted)`)
	gone := a.ok("UID EXPUNGE 2")
	has(t, gone, "* VANISHED 2")
	has(t, gone, "[HIGHESTMODSEQ ")
	lacks(t, gone, " EXPUNGE\r")
	restarted, err := mailbox.New(context.Background(), store, dir)
	if err != nil {
		t.Fatal(err)
	}
	r := syncWire(t, restarted)
	r.ok("ENABLE QRESYNC")
	out := r.ok(fmt.Sprintf("SELECT Drafts (QRESYNC (%d %d 1:3))", valid, h))
	has(t, out, "VANISHED (EARLIER) 2")
	has(t, out, "UID 1 FLAGS (finance) MODSEQ")
	lacks(t, out, "UID 3 FLAGS")
	if strings.Index(out, "VANISHED") > strings.Index(out, " FETCH ") {
		t.Fatal("vanished must precede fetch", out)
	}
	out = r.ok(fmt.Sprintf("UID FETCH 1:3 (FLAGS) (CHANGEDSINCE %d VANISHED)", h))
	has(t, out, "VANISHED (EARLIER) 2")
	has(t, out, "UID 1")
	lacks(t, out, "UID 3")
	out = r.ok(fmt.Sprintf("SELECT Drafts (QRESYNC (%d %d 3))", valid, h))
	lacks(t, out, "VANISHED")
	lacks(t, out, " FETCH ")
	out = r.ok(fmt.Sprintf("EXAMINE Drafts (QRESYNC (%d %d 1:3 (1:3 1:3)))", valid, h))
	has(t, out, "VANISHED (EARLIER) 2")
	has(t, r.cmd("UID STORE 1 +FLAGS (Other)"), " NO ")
	out = r.ok(fmt.Sprintf("SELECT Drafts (QRESYNC (%d %d))", valid+1, h))
	lacks(t, out, "VANISHED")
	lacks(t, out, " FETCH ")
}
func TestSyncLegacyAndMalformedCommands(t *testing.T) {
	e, _, _ := seeded(t)
	a := syncWire(t, e)
	out := a.cmd("SELECT Drafts (QRESYNC (1 1))")
	has(t, out, " BAD ")
	a.ok("SELECT Drafts")
	has(t, a.cmd("UID FETCH 1 (FLAGS) (CHANGEDSINCE 0 VANISHED)"), " BAD ")
	a.ok("ENABLE QRESYNC")
	has(t, a.cmd("FETCH 1 (FLAGS) (CHANGEDSINCE 0 VANISHED)"), " BAD ")
	has(t, a.cmd("UID FETCH 1 (FLAGS) (VANISHED)"), " BAD ")
	has(t, a.cmd("SELECT Drafts (QRESYNC (1 1 1:*))"), " BAD ")
	// Legacy client still sees sequence EXPUNGE, never VANISHED or MODSEQ.
	b := syncWire(t, e)
	out = b.ok("SELECT Drafts")
	lacks(t, out, "HIGHESTMODSEQ")
	b.ok(`UID STORE 2 +FLAGS (\Deleted)`)
	out = b.ok("EXPUNGE")
	has(t, out, "* 2 EXPUNGE")
	lacks(t, out, "VANISHED")
	lacks(t, out, "MODSEQ")
}
func TestQResyncDoesNotSkipDeferredExpunge(t *testing.T) {
	e, _, _ := seeded(t)
	a, b := syncWire(t, e), syncWire(t, e)
	a.ok("ENABLE QRESYNC")
	sel := a.ok("SELECT Drafts")
	h := number(t, sel, "HIGHESTMODSEQ")
	b.ok("SELECT Drafts")
	b.ok(`UID STORE 2 +FLAGS (\Deleted)`)
	b.ok("UID EXPUNGE 2")
	b.ok("UID STORE 3 +FLAGS (Finance)")
	// Sequence FETCH must retain old mapping until pending expunges are delivered.
	out := a.ok("FETCH 3 (FLAGS MODSEQ)")
	has(t, out, "UID 3")
	lacks(t, out, "VANISHED")
	limit := number(t, out, "HIGHESTMODSEQ")
	if limit <= h {
		t.Fatal(out)
	}
	out = a.ok("NOOP")
	has(t, out, "VANISHED 2")
	has(t, out, "UID 3 FLAGS (finance) MODSEQ")
}
