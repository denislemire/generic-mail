package sesmail

import (
	"context"
	"fmt"

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
		return fmt.Errorf("ses: %s", err.Error())
	}
	return nil
}
