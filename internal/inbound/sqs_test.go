package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeQueue struct {
	body                  string
	deleted               bool
	receiveErr, deleteErr error
	t                     *testing.T
}

func (f *fakeQueue) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if in.WaitTimeSeconds != 20 || in.VisibilityTimeout != 120 || in.MaxNumberOfMessages != 1 {
		f.t.Fatal("unexpected queue bounds")
	}
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{{Body: aws.String(f.body), ReceiptHandle: aws.String("receipt")}}}, f.receiveErr
}
func (f *fakeQueue) DeleteMessage(_ context.Context, _ *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleted = true
	return &sqs.DeleteMessageOutput{}, f.deleteErr
}

type fakeImporter struct {
	keys   []string
	failAt int
	queue  *fakeQueue
}

func (f *fakeImporter) SyncInboundKey(_ context.Context, key string, _ time.Time) error {
	if f.queue.deleted {
		return errors.New("acknowledged before import")
	}
	f.keys = append(f.keys, key)
	if f.failAt == len(f.keys) {
		return errors.New("commit failed")
	}
	return nil
}
func notification(keys ...string) string {
	var records []any
	for _, key := range keys {
		records = append(records, map[string]any{"eventSource": "aws:s3", "eventName": "ObjectCreated:Put", "eventTime": "2026-10-24T01:00:00Z", "s3": map[string]any{"bucket": map[string]string{"name": "mail"}, "object": map[string]string{"key": key}}})
	}
	b, _ := json.Marshal(map[string]any{"Records": records})
	return string(b)
}
func TestPollAcknowledgesOnlyCommittedRecords(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fail       int
		ack        bool
		keys       []string
	}{
		{name: "encoded keys", body: notification("inbound/a+b%2Bc"), ack: true, keys: []string{"inbound/a b+c"}},
		{name: "multiple records", body: notification("inbound/a", "inbound/b"), ack: true, keys: []string{"inbound/a", "inbound/b"}},
		{name: "partial failure", body: notification("inbound/a", "inbound/b"), fail: 2, keys: []string{"inbound/a", "inbound/b"}},
		{name: "test event", body: `{"Event":"s3:TestEvent","Service":"Amazon S3","Bucket":"mail"}`, ack: true},
		{name: "malformed", body: `{`},
		{name: "empty", body: `{}`},
		{name: "outside prefix", body: notification("sent/a")},
		{name: "bad escape", body: notification("inbound/%zz")},
		{name: "wrong bucket", body: `{"Event":"s3:TestEvent","Service":"Amazon S3","Bucket":"other"}`},
		{name: "directory marker", body: notification("inbound/folder/"), ack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{body: tc.body, t: t}
			i := &fakeImporter{queue: q, failAt: tc.fail}
			c := &Consumer{API: q, Importer: i, QueueURL: "queue", Bucket: "mail", Prefix: "inbound/"}
			err := c.Poll(context.Background())
			if (err == nil) != tc.ack || q.deleted != tc.ack || !reflect.DeepEqual(i.keys, tc.keys) {
				t.Fatalf("ack=%v err=%v keys=%v", q.deleted, err, i.keys)
			}
		})
	}
}
func TestQueueAPIFailures(t *testing.T) {
	for _, receive := range []bool{false, true} {
		q := &fakeQueue{body: notification("inbound/a"), t: t}
		if receive {
			q.receiveErr = errors.New("offline")
		} else {
			q.deleteErr = errors.New("offline")
		}
		c := &Consumer{API: q, Importer: &fakeImporter{queue: q}, Bucket: "mail", Prefix: "inbound/"}
		if c.Poll(context.Background()) == nil {
			t.Fatal("lost API error")
		}
	}
}
