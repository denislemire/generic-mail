package state

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
)

// ContentKey identifies immutable bytes independently of mailbox/UID. Copies
// receive new local file keys; flags and folder changes do not re-index bodies.
type ContentKey struct{ Loc, Ref string }

const maxIndexedBytes = 4 << 20

// IndexContent includes only referenced immutable messages. Oversized documents
// are marked incomplete so candidate selection always retains them for exact
// local verification; the index must never create false negatives.
func (d *DB) IndexContent(ctx context.Context, key ContentKey, raw []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO search_document (loc,ref,complete)
 SELECT ?,?,? WHERE EXISTS (SELECT 1 FROM message WHERE loc=? AND ref=?)`, key.Loc, key.Ref, len(raw) <= maxIndexedBytes, key.Loc, key.Ref)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if len(raw) <= maxIndexedBytes {
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		// Preserve all ASCII substrings, including those after invalid UTF-8 or NUL.
		// Candidate queries are ASCII-only and exact matching uses original bytes.
		content := strings.ReplaceAll(strings.ToValidUTF8(string(bytes.ToLower(raw)), " "), "\x00", " ")
		if _, err = tx.ExecContext(ctx, `INSERT INTO search_fts(rowid,content) VALUES (?,?)`, id, content); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (d *DB) ContentIndexed(ctx context.Context, key ContentKey) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var id int64
	err := d.sql.QueryRowContext(ctx, `SELECT id FROM search_document WHERE loc=? AND ref=?`, key.Loc, key.Ref).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// SearchCandidates is a superset. Missing/incomplete index rows are included,
// and callers verify the full IMAP expression against original bytes afterward.
func (d *DB) SearchCandidates(ctx context.Context, query string) (map[ContentKey]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.QueryContext(ctx, `SELECT d.loc,d.ref FROM search_fts JOIN search_document d ON d.id=search_fts.rowid WHERE search_fts MATCH ?
 UNION SELECT m.loc,m.ref FROM message m LEFT JOIN search_document d ON m.loc=d.loc AND m.ref=d.ref WHERE d.id IS NULL OR d.complete=0`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ContentKey]bool{}
	for rows.Next() {
		var k ContentKey
		if err = rows.Scan(&k.Loc, &k.Ref); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

const searchSchema = `
CREATE INDEX IF NOT EXISTS message_content_ref ON message(loc,ref);
CREATE TABLE IF NOT EXISTS search_document (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 loc TEXT NOT NULL,
 ref TEXT NOT NULL,
 complete INTEGER NOT NULL,
 UNIQUE(loc,ref)
);
CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(content,content='',contentless_delete=1,tokenize='trigram case_sensitive 1');
CREATE TRIGGER IF NOT EXISTS search_document_delete AFTER DELETE ON search_document BEGIN
 DELETE FROM search_fts WHERE rowid=old.id;
END;
`

func (d *DB) SearchStats(ctx context.Context) (complete, fallback int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err = d.sql.QueryRowContext(ctx, `SELECT coalesce(sum(complete),0),coalesce(sum(1-complete),0) FROM search_document`).Scan(&complete, &fallback)
	return
}
