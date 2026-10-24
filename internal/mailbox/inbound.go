package mailbox

import (
	"context"
	"errors"
	"time"

	"github.com/denislemire/generic-mail/internal/blob"
	"github.com/emersion/go-imap/v2"
)

// SyncInboundKey imports an event's object without listing the bucket. The
// second seen check and SQLite commit serialize this with reconciliation and
// preserve UIDs when S3/SQS delivers a duplicate or out-of-order notification.
func (e *Engine) SyncInboundKey(ctx context.Context, key string, when time.Time) error {
	e.mu.Lock()
	_, exists := e.seen[key]
	e.mu.Unlock()
	if exists {
		return nil
	}
	var raw []byte
	if !isSetupNotice(key) {
		var err error
		raw, err = e.blobs.Get(ctx, key)
		if errors.Is(err, blob.ErrNotFound) {
			return nil
		} // Removed before delivery.
		if err != nil {
			return err
		}
	}
	if when.IsZero() {
		when = time.Now()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.mutateLocked(func() error {
		if _, exists := e.seen[key]; exists {
			return nil
		}
		if isSetupNotice(key) {
			e.seen[key] = struct{}{}
			return nil
		}
		e.addInboundLocked(key, raw, when)
		e.queueCount(e.boxes[BoxINBOX])
		return nil
	})
	if err == nil && raw != nil {
		e.rememberLocked(locS3, key, raw)
	}
	return err
}

func (e *Engine) addInboundLocked(key string, raw []byte, when time.Time) {
	inbox := e.boxes[BoxINBOX]
	inbox.msgs = append(inbox.msgs, &message{uid: inbox.uidNext, loc: locS3, key: key, source: key, size: int64(len(raw)), internal: when.UTC(), flags: map[imap.Flag]struct{}{}})
	inbox.uidNext++
	e.seen[key] = struct{}{}
}
