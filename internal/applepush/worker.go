package applepush

import (
	"context"
	"log"
	"time"

	"github.com/denislemire/generic-mail/internal/state"
)

type Queue interface {
	PendingPush(context.Context, time.Time, int) ([]state.PushJob, error)
	CompletePush(context.Context, state.PushJob, bool) error
	RetryPush(context.Context, state.PushJob, time.Time) error
}
type Sender interface {
	Send(context.Context, state.PushJob) (Result, error)
}

type Worker struct {
	Queue  Queue
	Sender Sender
	Now    func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Drain(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Apple push queue: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (w *Worker) Drain(ctx context.Context) error {
	jobs, err := w.Queue.PendingPush(ctx, w.now(), 32)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err = ctx.Err(); err != nil {
			return err
		}
		result, sendErr := w.Sender.Send(ctx, job)
		if sendErr == nil {
			if err = w.Queue.CompletePush(ctx, job, result.Gone); err != nil {
				return err
			}
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := retryDelay(job.Attempts)
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			if err = w.Queue.RetryPush(ctx, job, w.now().Add(delay)); err != nil {
				return err
			}
			log.Printf("Apple push delivery deferred: %v", sendErr)
		}
	}
	return nil
}
func retryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 8 {
		attempts = 8
	}
	delay := 5 * time.Second * time.Duration(1<<attempts)
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	return delay
}
