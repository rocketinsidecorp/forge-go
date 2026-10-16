// Package mail builds and delivers email.
//
// A Message is plain data that is validated and encoded as MIME by Encode, so
// header injection, recipient smuggling and malformed MIME are rejected
// before any bytes reach a transport. Delivery goes through the Sender port.
// Forge ships a TLS-enforcing SMTP sender, a development sender that writes
// .eml files, an in-memory recorder (package mailtest) for tests and an HTML
// preview handler. Typed templates render the bodies.
//
// The package depends only on the standard library and other Forge core
// packages (ADR 0006). Queueing is deliberately not part of it: a queue is
// just another Sender (see docs/MAIL.md).
package mail

import (
	"context"
	"net/http"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Sender delivers one message. Implementations must be safe for concurrent
// use, validate the message (Encode does) before any side effect, and honour
// ctx cancellation. A nil error means the message was accepted for delivery,
// not that it reached the inbox.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, msg Message) error

// Send calls fn.
func (fn SenderFunc) Send(ctx context.Context, msg Message) error { return fn(ctx, msg) }

// Delivery faults. Errors never contain credentials, recipient addresses or
// server reply text; Metadata carries the SMTP stage and reply code only.
// Match them with errors.Is.
var (
	// ErrUnavailable is a transient failure (network, timeout, 4xx reply,
	// cancellation). Retrying later may succeed.
	ErrUnavailable = &fault.Error{Code: "mail_unavailable", Message: "mail delivery is temporarily unavailable", Category: fault.CategoryUnavailable, Retryable: true}
	// ErrRejected is a permanent refusal by the server (5xx reply, refused
	// recipient). Retrying the same message will not help.
	//
	// It is a failure of the upstream mail system, not of the caller's HTTP
	// request, so it maps to 502 rather than 400.
	ErrRejected = fault.New("mail_rejected", "mail server rejected the message", fault.CategoryUnavailable, http.StatusBadGateway)
	// ErrAuthFailed means the SMTP credentials were refused or no safe
	// authentication mechanism is available.
	ErrAuthFailed = fault.New("mail_auth_failed", "mail server authentication failed", fault.CategoryInternal, 0)
	// ErrTLSRequired means the server does not offer STARTTLS and plaintext
	// is not allowed, so nothing was sent.
	ErrTLSRequired = fault.New("mail_tls_required", "mail server does not offer TLS", fault.CategoryInternal, 0)
	// ErrTLSFailed means the TLS handshake or certificate verification failed.
	ErrTLSFailed = fault.New("mail_tls_failed", "mail server TLS handshake failed", fault.CategoryInternal, 0)
)
