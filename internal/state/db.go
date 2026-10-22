// Package state is the local IMAP index. Message bodies are not stored here.
package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Box is one mailbox in the index.
type Box struct {
	Name        string
	UIDValidity uint32
	UIDNext     uint32
	Subscribed  bool
	Messages    []Msg
}

// Msg is one index row. Loc is "s3" or "file".
type Msg struct {
	UID      uint32
	Loc      string
	Ref      string
	Source   string
	Size     int64
	Internal time.Time
	Flags    []string
}

// Snap is a full index.
type Snap struct {
	Boxes []Box
	Seen  []string
}

// DB is the SQLite index.
type DB struct {
	sql *sql.DB
}

// Open creates the schema if needed.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Empty reports whether any mailbox row exists.
func (d *DB) Empty() (bool, error) {
	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM mailbox`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// Load reads the index.
func (d *DB) Load() (Snap, error) {
	var snap Snap
	rows, err := d.sql.Query(`SELECT name, uid_validity, uid_next, subscribed FROM mailbox`)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Box
		var sub int
		if err := rows.Scan(&b.Name, &b.UIDValidity, &b.UIDNext, &sub); err != nil {
			return snap, err
		}
		b.Subscribed = sub != 0
		snap.Boxes = append(snap.Boxes, b)
	}
	if err := rows.Err(); err != nil {
		return snap, err
	}
	byName := map[string]*Box{}
	for i := range snap.Boxes {
		byName[snap.Boxes[i].Name] = &snap.Boxes[i]
	}
	mrows, err := d.sql.Query(`SELECT mailbox, uid, loc, ref, source_key, size, internal, flags FROM message ORDER BY mailbox, uid`)
	if err != nil {
		return snap, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var mailbox, loc, ref, source, internal, flags string
		var uid uint32
		var size int64
		if err := mrows.Scan(&mailbox, &uid, &loc, &ref, &source, &size, &internal, &flags); err != nil {
			return snap, err
		}
		box := byName[mailbox]
		if box == nil {
			continue
		}
		when, err := time.Parse(time.RFC3339Nano, internal)
		if err != nil {
			return snap, fmt.Errorf("message time: %w", err)
		}
		var flagList []string
		if flags != "" {
			if err := json.Unmarshal([]byte(flags), &flagList); err != nil {
				return snap, err
			}
		}
		box.Messages = append(box.Messages, Msg{
			UID: uid, Loc: loc, Ref: ref, Source: source, Size: size, Internal: when, Flags: flagList,
		})
	}
	if err := mrows.Err(); err != nil {
		return snap, err
	}
	srows, err := d.sql.Query(`SELECT key FROM seen`)
	if err != nil {
		return snap, err
	}
	defer srows.Close()
	for srows.Next() {
		var key string
		if err := srows.Scan(&key); err != nil {
			return snap, err
		}
		snap.Seen = append(snap.Seen, key)
	}
	return snap, srows.Err()
}

// Save replaces the index in one transaction.
func (d *DB) Save(snap Snap) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM message`, `DELETE FROM mailbox`, `DELETE FROM seen`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	for _, b := range snap.Boxes {
		sub := 0
		if b.Subscribed {
			sub = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO mailbox (name, uid_validity, uid_next, subscribed) VALUES (?, ?, ?, ?)`,
			b.Name, b.UIDValidity, b.UIDNext, sub,
		); err != nil {
			return err
		}
		for _, m := range b.Messages {
			flags, err := json.Marshal(m.Flags)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`INSERT INTO message (mailbox, uid, loc, ref, source_key, size, internal, flags) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				b.Name, m.UID, m.Loc, m.Ref, m.Source, m.Size, m.Internal.UTC().Format(time.RFC3339Nano), string(flags),
			); err != nil {
				return err
			}
		}
	}
	for _, key := range snap.Seen {
		if _, err := tx.Exec(`INSERT INTO seen (key) VALUES (?)`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS mailbox (
  name TEXT PRIMARY KEY,
  uid_validity INTEGER NOT NULL,
  uid_next INTEGER NOT NULL,
  subscribed INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message (
  mailbox TEXT NOT NULL,
  uid INTEGER NOT NULL,
  loc TEXT NOT NULL,
  ref TEXT NOT NULL,
  source_key TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL,
  internal TEXT NOT NULL,
  flags TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (mailbox, uid)
);
CREATE TABLE IF NOT EXISTS seen (
  key TEXT PRIMARY KEY
);
`
