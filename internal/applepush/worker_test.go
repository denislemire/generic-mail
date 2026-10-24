package applepush

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/denislemire/generic-mail/internal/state"
)

type senderFunc func(context.Context, state.PushJob) (Result, error)

func (f senderFunc) Send(ctx context.Context, job state.PushJob) (Result, error) { return f(ctx, job) }
func TestWorkerRetryAndDelivery(t *testing.T) {
	ctx := context.Background()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.RegisterPush(ctx, "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	calls := 0
	worker := &Worker{Queue: db, Now: func() time.Time { return now }, Sender: senderFunc(func(context.Context, state.PushJob) (Result, error) {
		calls++
		if calls == 1 {
			return Result{RetryAfter: time.Minute}, errors.New("throttled")
		}
		return Result{}, nil
	})}
	if err = worker.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err = worker.Drain(ctx); err != nil || calls != 1 {
		t.Fatal("backoff was bypassed")
	}
	now = now.Add(2 * time.Minute)
	if err = worker.Drain(ctx); err != nil || calls != 2 {
		t.Fatal("retry did not deliver")
	}
	jobs, err := db.PendingPush(ctx, now, 64)
	if err != nil || len(jobs) != 0 {
		t.Fatal("delivered hint not acknowledged")
	}
}
func TestWorkerCancellationKeepsPendingHint(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.RegisterPush(context.Background(), "account", "token", []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Worker{Queue: db, Sender: senderFunc(func(context.Context, state.PushJob) (Result, error) { cancel(); return Result{}, context.Canceled })}
	if err = worker.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated")
	}
	jobs, err := db.PendingPush(context.Background(), time.Now(), 64)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 0 {
		t.Fatal("cancellation lost or delayed hint")
	}
}
