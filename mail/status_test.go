package mail_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// A refusal by the mail server is not a client error: surfacing it as 400
// would blame the caller of an HTTP endpoint for an upstream condition.
func TestDeliveryFaultsMapToServerSideHTTPStatuses(t *testing.T) {
	t.Parallel()
	want := map[*fault.Error]int{
		forgemail.ErrRejected:    http.StatusBadGateway,
		forgemail.ErrUnavailable: http.StatusServiceUnavailable,
		forgemail.ErrAuthFailed:  http.StatusInternalServerError,
		forgemail.ErrTLSRequired: http.StatusInternalServerError,
		forgemail.ErrTLSFailed:   http.StatusInternalServerError,
		forgemail.ErrTemplate:    http.StatusInternalServerError,
	}
	for err, status := range want {
		if got := err.Status(); got != status {
			t.Errorf("%s status = %d, want %d", err.Code, got, status)
		}
		if err.Category == fault.CategoryInvalid {
			t.Errorf("%s must not be categorised as a client error", err.Code)
		}
	}
	if forgemail.ErrRejected.Retryable {
		t.Error("a permanent refusal must not be retryable")
	}
}

// 530 and 535 mean the server wants (valid) authentication, whichever
// command they answer.
func TestAuthenticationRepliesAtAnyStageMapToAuthFailed(t *testing.T) {
	t.Parallel()
	cert, pool := testCert(t)
	for verb, reply := range map[string]string{
		"MAIL": "530 5.7.0 Authentication required",
		"RCPT": "535 5.7.8 Authentication credentials invalid",
		"DATA": "530 5.7.0 Must issue a STARTTLS command first",
	} {
		server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.replies[verb] = reply })
		err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage())
		if !errors.Is(err, forgemail.ErrAuthFailed) {
			t.Errorf("%s %q: err = %v, want ErrAuthFailed", verb, reply, err)
			continue
		}
		assertNoSecrets(t, err)
		if fault.From(err).Status() == http.StatusBadRequest {
			t.Errorf("%s: 400 for a server-side failure", verb)
		}
	}
	// Other permanent refusals stay ErrRejected.
	server := newFakeSMTPWithCert(t, cert, func(s *fakeSMTP) { s.replies["RCPT"] = "550 5.1.1 no such user" })
	if err := newSender(t, server, pool, nil).Send(context.Background(), sendMessage()); !errors.Is(err, forgemail.ErrRejected) {
		t.Fatalf("err = %v", err)
	}
}
