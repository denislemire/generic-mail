package sesmail

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/smithy-go"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Client sends raw messages through SES API v2.
type Client struct {
	api *sesv2.Client
}

// New loads the default credential chain in region. generic.business send is ca-central-1.
// The pod uses the projected service-account token, not an access key.
func New(ctx context.Context, region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &Client{api: sesv2.NewFromConfig(cfg)}, nil
}

// SendRaw submits the RFC822 message through the SES API in the configured region.
func (c *Client) SendRaw(ctx context.Context, from string, rcpts []string, raw []byte) error {
	_, err := c.api.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from),
		Destination:      &types.Destination{ToAddresses: rcpts},
		Content: &types.EmailContent{
			Raw: &types.RawMessage{Data: raw},
		},
	})
	if err != nil {
		return classifySendError(err)
	}
	return nil
}

// Only explicit content/address rejections are permanent. Transport failures,
// throttling, service failures, and account configuration problems can be retried.
func classifySendError(err error) error {
	permanent := false
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "MessageRejected", "BadRequestException", "MailFromDomainNotVerifiedException", "NotFoundException":
			permanent = true
		}
	}
	return &sendError{cause: fmt.Errorf("ses: %w", err), permanent: permanent}
}

type sendError struct {
	cause     error
	permanent bool
}

func (e *sendError) Error() string   { return e.cause.Error() }
func (e *sendError) Unwrap() error   { return e.cause }
func (e *sendError) Permanent() bool { return e.permanent }
