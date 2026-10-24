package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PushJob is a coalesced mailbox-change hint for one registered device/account.
// DeviceToken and AccountID must never be written to logs.
type PushJob struct {
	ID                  int64
	AccountID           string
	DeviceToken         string
	RegistrationVersion int64
	Generation          int64
	Attempts            int
}

// RegisterPush persists a registration and its initial synchronization hint.
// The account/device pair permits several devices for the same Mail account.
func (d *DB) RegisterPush(ctx context.Context, account, token string, mailboxes []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM push_registration WHERE account_id=? AND device_token=?`, account, token).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM push_registration`).Scan(&count); err != nil {
			return err
		}
		if count >= 256 {
			return errors.New("push registration limit reached")
		}
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO push_registration (account_id,device_token,revision) VALUES (?,?,1)`, account, token)
		if insertErr != nil {
			return insertErr
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE push_registration SET revision=revision+1 WHERE id=?`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM push_mailbox WHERE registration_id=?`, id); err != nil {
		return err
	}
	for _, name := range mailboxes {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO push_mailbox (registration_id,mailbox) VALUES (?,?)`, id, name); err != nil {
			return err
		}
	}
	if len(mailboxes) == 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM push_pending WHERE registration_id=?`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO push_pending (registration_id,generation,attempts,next_attempt) VALUES (?,1,0,0)
 ON CONFLICT(registration_id) DO UPDATE SET generation=push_pending.generation+1,attempts=0,next_attempt=0`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) PendingPush(ctx context.Context, now time.Time, limit int) ([]PushJob, error) {
	if limit < 1 || limit > 64 {
		return nil, fmt.Errorf("invalid push batch limit")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.sql.QueryContext(ctx, `SELECT r.id,r.account_id,r.device_token,r.revision,p.generation,p.attempts FROM push_pending p
 JOIN push_registration r ON r.id=p.registration_id WHERE p.next_attempt<=? ORDER BY p.next_attempt,r.id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []PushJob
	for rows.Next() {
		var job PushJob
		if err = rows.Scan(&job.ID, &job.AccountID, &job.DeviceToken, &job.RegistrationVersion, &job.Generation, &job.Attempts); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CompletePush acknowledges only the version sent. A concurrent mailbox change
// or device re-registration keeps its newer pending notification.
func (d *DB) CompletePush(ctx context.Context, job PushJob, unregistered bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if unregistered {
		var revision int64
		if err = tx.QueryRowContext(ctx, `SELECT revision FROM push_registration WHERE id=?`, job.ID).Scan(&revision); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if revision != job.RegistrationVersion {
			return nil
		}
		for _, table := range []string{"push_pending", "push_mailbox"} {
			if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE registration_id=?`, job.ID); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM push_registration WHERE id=?`, job.ID); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `DELETE FROM push_pending WHERE registration_id=? AND generation=? AND EXISTS (SELECT 1 FROM push_registration WHERE id=? AND revision=?)`, job.ID, job.Generation, job.ID, job.RegistrationVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) RetryPush(ctx context.Context, job PushJob, next time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.sql.ExecContext(ctx, `UPDATE push_pending SET attempts=attempts+1,next_attempt=? WHERE registration_id=? AND generation=? AND EXISTS (SELECT 1 FROM push_registration WHERE id=? AND revision=?)`, next.Unix(), job.ID, job.Generation, job.ID, job.RegistrationVersion)
	return err
}

// enqueuePush is part of the mailbox's existing SQLite transaction. Generation
// increments prevent acknowledgements from losing newer changes during an APNs call.
func enqueuePush(tx *sql.Tx, mailbox string) error {
	_, err := tx.Exec(`INSERT INTO push_pending (registration_id,generation,attempts,next_attempt)
 SELECT DISTINCT registration_id,1,0,0 FROM push_mailbox WHERE mailbox=?
 ON CONFLICT(registration_id) DO UPDATE SET generation=push_pending.generation+1`, mailbox)
	return err
}

const pushSchema = `
CREATE TABLE IF NOT EXISTS push_registration (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 account_id TEXT NOT NULL,
 device_token TEXT NOT NULL,
 revision INTEGER NOT NULL,
 UNIQUE(account_id,device_token)
);
CREATE TABLE IF NOT EXISTS push_mailbox (
 registration_id INTEGER NOT NULL,
 mailbox TEXT NOT NULL,
 PRIMARY KEY(registration_id,mailbox)
);
CREATE INDEX IF NOT EXISTS push_mailbox_name ON push_mailbox(mailbox);
CREATE TABLE IF NOT EXISTS push_pending (
 registration_id INTEGER PRIMARY KEY AUTOINCREMENT,
 generation INTEGER NOT NULL,
 attempts INTEGER NOT NULL,
 next_attempt INTEGER NOT NULL
);
`
