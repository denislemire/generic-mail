package sesmail

import (
	"errors"
	"github.com/aws/smithy-go"
	"testing"
)

func TestDeliveryErrorClassification(t *testing.T) {
	for _, code := range []string{"MessageRejected", "BadRequestException", "MailFromDomainNotVerifiedException", "NotFoundException", "TooManyRequestsException", "LimitExceededException", "InternalServerErrorException", "SendingPausedException"} {
		cause := &smithy.GenericAPIError{Code: code, Message: "provider detail"}
		err := classifySendError(cause)
		var classification interface{ Permanent() bool }
		if !errors.As(err, &classification) {
			t.Fatal("missing classification")
		}
		permanent := code == "MessageRejected" || code == "BadRequestException" || code == "MailFromDomainNotVerifiedException" || code == "NotFoundException"
		if classification.Permanent() != permanent || !errors.Is(err, cause) {
			t.Errorf("wrong classification for %s", code)
		}
	}
}
