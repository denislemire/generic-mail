package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSaveTouchesOnlyChangedRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap := Snap{Boxes: []Box{{Name: "INBOX", UIDValidity: 1, UIDNext: 3, Messages: []Msg{
		{UID: 1, Loc: "s3", Ref: "one", Internal: time.Now().UTC()},
		{UID: 2, Loc: "s3", Ref: "two", Internal: time.Now().UTC()},
	}}}, Seen: []string{"one", "two"}}
	if err = db.Save(snap); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`CREATE TABLE audit (operation TEXT);
 CREATE TRIGGER message_insert AFTER INSERT ON message BEGIN INSERT INTO audit VALUES ('insert'); END;
 CREATE TRIGGER message_update AFTER UPDATE ON message BEGIN INSERT INTO audit VALUES ('update'); END;
 CREATE TRIGGER message_delete AFTER DELETE ON message BEGIN INSERT INTO audit VALUES ('delete'); END;
 CREATE TRIGGER mailbox_update AFTER UPDATE ON mailbox BEGIN INSERT INTO audit VALUES ('mailbox'); END;
 CREATE TRIGGER seen_insert AFTER INSERT ON seen BEGIN INSERT INTO audit VALUES ('seen'); END;`); err != nil {
		t.Fatal(err)
	}
	snap.Boxes[0].Messages[0].Flags = []string{`\Seen`}
	if err = db.Save(snap); err != nil {
		t.Fatal(err)
	}
	var n int
	var operation string
	if err = db.sql.QueryRow(`SELECT count(*),min(operation) FROM audit`).Scan(&n, &operation); err != nil {
		t.Fatal(err)
	}
	if n != 1 || operation != "update" {
		t.Fatalf("unexpected writes: %d %s", n, operation)
	}
	if err = db.Save(snap); err != nil {
		t.Fatal(err)
	}
	if err = db.sql.QueryRow(`SELECT count(*) FROM audit`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unchanged snapshot wrote rows: %d %v", n, err)
	}
}

func TestFailedSaveCanBeRetried(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap := Snap{Boxes: []Box{{Name: "Drafts", UIDValidity: 1, UIDNext: 1}}}
	if err = db.Save(snap); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER fail BEFORE UPDATE ON mailbox BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	snap.Boxes[0].UIDNext = 2
	if err = db.Save(snap); err == nil {
		t.Fatal("expected failure")
	}
	if _, err = db.sql.Exec(`DROP TRIGGER fail`); err != nil {
		t.Fatal(err)
	}
	if err = db.Save(snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.Load()
	if err != nil || loaded.Boxes[0].UIDNext != 2 {
		t.Fatalf("retry lost: %+v %v", loaded, err)
	}
}
