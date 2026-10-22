// Package blob is the object store behind the interim mailbox.
// Inbound mail stays as the raw MIME objects SES already wrote. IMAP flags and
// UID assignment live in one JSON state object so a pod restart does not
// renumber the mailbox.
package blob

import (
	"context"
	"errors"
	"time"
)

const StateKey = "imap-state/mailboxes.json"

// ErrNotFound is returned when a key or the state object does not exist.
var ErrNotFound = errors.New("blob: not found")

// ErrPrecondition is returned when a conditional state write loses a race.
var ErrPrecondition = errors.New("blob: precondition failed")

// Meta is a listed object, without its body.
type Meta struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the subset of S3 this process uses.
type Store interface {
	List(ctx context.Context, prefix string) ([]Meta, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, body []byte) error
	Delete(ctx context.Context, key string) error
	// GetState returns ErrNotFound when the state object has never been written.
	// etag is opaque and round-tripped into PutState.
	GetState(ctx context.Context, key string) (body []byte, etag string, err error)
	// PutState writes state. A non-empty etag must match the current object.
	// An empty etag creates the object, or overwrites when the implementation
	// has no prior etag (first boot).
	PutState(ctx context.Context, key string, body []byte, etag string) (newETag string, err error)
}
