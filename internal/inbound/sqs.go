// Package inbound consumes direct S3 ObjectCreated notifications from SQS.
package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type API interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}
type Importer interface {
	SyncInboundKey(context.Context, string, time.Time) error
}
type Consumer struct {
	API                      API
	Importer                 Importer
	QueueURL, Bucket, Prefix string
}

func New(ctx context.Context, queueURL, region, bucket, prefix string, importer Importer) (*Consumer, error) {
	u, err := url.Parse(queueURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("SQS_QUEUE_URL must be an HTTPS queue URL")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &Consumer{API: sqs.NewFromConfig(cfg), Importer: importer, QueueURL: queueURL, Bucket: bucket, Prefix: prefix}, nil
}

func (c *Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.Poll(ctx); err != nil && ctx.Err() == nil {
			// Do not log queue bodies or SDK errors, which can include receipt handles.
			log.Printf("inbound queue poll failed; retrying in 5s")
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// Poll receives one notification at a time. Only acknowledge after every object
// in it has committed. Crashes and partial failures are retried by SQS; the
// mailbox's durable seen keys deduplicate objects already committed.
func (c *Consumer) Poll(ctx context.Context) error {
	out, err := c.API.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.QueueURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 20, VisibilityTimeout: 120})
	if err != nil {
		return err
	}
	for _, m := range out.Messages {
		if err := c.importEvent(ctx, aws.ToString(m.Body)); err != nil {
			return err
		}
		if m.ReceiptHandle == nil {
			return errors.New("SQS response lacks receipt handle")
		}
		if _, err := c.API.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.QueueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
			return err
		}
	}
	return nil
}

type event struct {
	Event   string `json:"Event"`
	Service string `json:"Service"`
	Bucket  string `json:"Bucket"`
	Records []struct {
		Source string    `json:"eventSource"`
		Name   string    `json:"eventName"`
		Time   time.Time `json:"eventTime"`
		S3     struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key string `json:"key"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

func (c *Consumer) importEvent(ctx context.Context, body string) error {
	var e event
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return errors.New("invalid S3 notification")
	}
	if e.Event == "s3:TestEvent" && e.Service == "Amazon S3" && e.Bucket == c.Bucket {
		return nil
	}
	if len(e.Records) == 0 {
		return errors.New("S3 notification has no records")
	}
	for _, r := range e.Records {
		if r.Source != "aws:s3" || !strings.HasPrefix(r.Name, "ObjectCreated:") || r.S3.Bucket.Name != c.Bucket {
			return errors.New("unexpected S3 notification source")
		}
		key, err := url.QueryUnescape(r.S3.Object.Key)
		if err != nil || key == "" || !strings.HasPrefix(key, c.Prefix) {
			return errors.New("unexpected S3 notification key")
		}
		if strings.HasSuffix(key, "/") {
			continue
		}
		// Each import is bounded below the visibility timeout. Expiry can still
		// cause concurrent delivery; durable deduplication makes that safe.
		itemCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err = c.Importer.SyncInboundKey(itemCtx, key, r.Time)
		cancel()
		if err != nil {
			return fmt.Errorf("import S3 notification: %w", err)
		}
	}
	return nil
}
