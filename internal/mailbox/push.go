package mailbox

import (
	"context"
	"time"

	"github.com/denislemire/generic-mail/internal/state"
)

func (e *Engine) RegisterPush(ctx context.Context, account, token string, mailboxes []string) error {
	return e.db.RegisterPush(ctx, account, token, mailboxes)
}
func (e *Engine) PendingPush(ctx context.Context, now time.Time, limit int) ([]state.PushJob, error) {
	return e.db.PendingPush(ctx, now, limit)
}
func (e *Engine) CompletePush(ctx context.Context, job state.PushJob, unregistered bool) error {
	return e.db.CompletePush(ctx, job, unregistered)
}
func (e *Engine) RetryPush(ctx context.Context, job state.PushJob, next time.Time) error {
	return e.db.RetryPush(ctx, job, next)
}
