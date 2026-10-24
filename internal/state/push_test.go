package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func pushDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func pending(t *testing.T, d *DB) []PushJob {
	t.Helper()
	jobs, err := d.PendingPush(context.Background(), time.Now(), 64)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}
func TestPushMultipleDevicesAndStaleAcknowledgements(t *testing.T) {
	ctx := context.Background()
	d := pushDB(t)
	for _, token := range []string{"phone", "tablet"} {
		if err := d.RegisterPush(ctx, "same-account", token, []string{"INBOX"}); err != nil {
			t.Fatal(err)
		}
	}
	jobs := pending(t, d)
	if len(jobs) != 2 {
		t.Fatal("one device replaced another")
	}
	old := jobs[0]
	if err := d.RegisterPush(ctx, old.AccountID, old.DeviceToken, []string{"INBOX", "Sent"}); err != nil {
		t.Fatal(err)
	}
	if err := d.CompletePush(ctx, old, true); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 2 {
		t.Fatal("stale invalid-token response removed newer registration")
	}
	if err := d.CompletePush(ctx, old, false); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 2 {
		t.Fatal("stale acknowledgement removed newer hint")
	}
	jobs = pending(t, d)
	if err := d.CompletePush(ctx, jobs[0], true); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 1 {
		t.Fatal("invalid device cleanup affected another registration")
	}
}
func TestPushIsAtomicWithMailboxAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	d := pushDB(t)
	snap := Snap{Boxes: []Box{{Name: "INBOX", UIDValidity: 1, UIDNext: 1}, {Name: "Drafts", UIDValidity: 2, UIDNext: 1}}}
	if err := d.Save(snap); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterPush(ctx, "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	if err := d.CompletePush(ctx, pending(t, d)[0], false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER fail BEFORE INSERT ON message BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	snap.Boxes[0].UIDNext = 2
	snap.Boxes[0].Messages = []Msg{{UID: 1, Loc: "s3", Ref: "one", Internal: time.Now().UTC()}}
	if err := d.Save(snap); err == nil {
		t.Fatal("expected save failure")
	}
	if len(pending(t, d)) != 0 {
		t.Fatal("failed transaction leaked notification")
	}
	if _, err := d.sql.Exec(`DROP TRIGGER fail`); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(snap); err != nil {
		t.Fatal(err)
	}
	old := pending(t, d)[0]
	// A second mailbox change during delivery must survive the first acknowledgement.
	snap.Boxes[0].Messages[0].Flags = []string{`\Seen`}
	if err := d.Save(snap); err != nil {
		t.Fatal(err)
	}
	if err := d.CompletePush(ctx, old, false); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 1 {
		t.Fatal("newer mailbox update was lost")
	}
	// The outbox is in SQLite, independent of process state.
	another, err := d.PendingPush(ctx, time.Now(), 64)
	if err != nil || len(another) != 1 {
		t.Fatal("pending delivery missing")
	}
	reopened, err := Open(d.sqlPathForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(pending(t, reopened)) != 1 {
		t.Fatal("pending delivery did not survive reopen")
	}
}

// Query SQLite for its own filename; no state dump or message content is read.
func (d *DB) sqlPathForTest(t *testing.T) string {
	t.Helper()
	var seq int
	var name, path string
	if err := d.sql.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestPushRetryAndUnsubscribe(t *testing.T) {
	ctx := context.Background()
	d := pushDB(t)
	if err := d.RegisterPush(ctx, "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	job := pending(t, d)[0]
	if err := d.RetryPush(ctx, job, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 0 {
		t.Fatal("retry backoff ignored")
	}
	jobs, err := d.PendingPush(ctx, time.Now().Add(2*time.Hour), 64)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 {
		t.Fatal("retry state not persisted")
	}
	if err = d.RegisterPush(ctx, "account", "token", nil); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, d)) != 0 {
		t.Fatal("empty subscriptions did not cancel hints")
	}
}

func TestPushUnsubscribeReregisterDuringDelivery(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.RegisterPush(ctx, "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	old, err := d.PendingPush(ctx, time.Now(), 32)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RegisterPush(ctx, "account", "token", nil); err != nil {
		t.Fatal(err)
	}
	if err = d.RegisterPush(ctx, "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	if err = d.RetryPush(ctx, old[0], time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = d.CompletePush(ctx, old[0], false); err != nil {
		t.Fatal(err)
	}
	pending, err := d.PendingPush(ctx, time.Now(), 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Attempts != 0 {
		t.Fatal("stale delivery altered new registration")
	}
}
