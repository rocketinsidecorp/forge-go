// Package mailtest provides an in-memory mail.Sender for application tests,
// the equivalent of ActionMailer::Base.deliveries.
//
// The recorder runs the real encoder on every message, so a test fails on
// exactly the messages production would refuse (header injection, missing
// recipients, oversized attachments) rather than recording invalid mail.
package mailtest

import (
	"bytes"
	"context"
	"net/mail"
	"slices"
	"sync"
	"time"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// Delivery is one recorded message.
type Delivery struct {
	// Message is a deep copy of what was sent.
	Message forgemail.Message
	// Envelope is the SMTP envelope: sender, recipients (including Bcc) and
	// the Message-ID.
	Sender     string
	Recipients []string
	MessageID  string
	// Raw is the exact RFC 5322 text the SMTP sender would transmit.
	Raw []byte
	// At is when the message was recorded.
	At time.Time
}

// Parse re-reads Raw with net/mail, for assertions on real headers.
func (d Delivery) Parse() (*mail.Message, error) {
	return mail.ReadMessage(bytes.NewReader(d.Raw))
}

// Recorder collects deliveries. The zero value is ready to use and safe for
// concurrent use.
type Recorder struct {
	mu         sync.Mutex
	deliveries []Delivery
	err        error
}

var _ forgemail.Sender = (*Recorder)(nil)

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Send validates msg like production and records it, unless a failure was
// configured with FailWith.
func (r *Recorder) Send(ctx context.Context, msg forgemail.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := forgemail.Encode(msg)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.deliveries = append(r.deliveries, Delivery{
		Message:    msg.Clone(),
		Sender:     encoded.Sender,
		Recipients: encoded.Recipients,
		MessageID:  encoded.MessageID,
		Raw:        encoded.Data,
		At:         time.Now(),
	})
	return nil
}

// Deliveries returns a snapshot of everything recorded, oldest first.
func (r *Recorder) Deliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Delivery, len(r.deliveries))
	for i, d := range r.deliveries {
		d.Message = d.Message.Clone()
		d.Raw = bytes.Clone(d.Raw)
		d.Recipients = slices.Clone(d.Recipients)
		out[i] = d
	}
	return out
}

// Len reports how many messages were recorded.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deliveries)
}

// Last returns the most recent delivery.
func (r *Recorder) Last() (Delivery, bool) {
	deliveries := r.Deliveries()
	if len(deliveries) == 0 {
		return Delivery{}, false
	}
	return deliveries[len(deliveries)-1], true
}

// FailWith makes every following Send return err without recording, to test
// error handling. FailWith(nil) restores normal behaviour.
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Reset forgets all deliveries and any configured failure.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = nil
	r.err = nil
}
