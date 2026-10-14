package realtime

import (
	"context"

	"github.com/fabriciobonjorno/forge-go/uuid"
)

// Envelope is an event on its way between replicas.
type Envelope struct {
	// Origin identifies the broker that published the event; receivers ignore
	// their own envelopes, so a transport may echo them.
	Origin uuid.UUID `json:"origin"`
	// Scope is "global" or the tenant UUID the topic belongs to. Receivers
	// route by it, so a replica never needs to trust the topic name alone.
	Scope     string `json:"scope"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
	Event     Event  `json:"event"`
}

// Receiver is where a Transport delivers what it hears from other replicas.
type Receiver interface {
	// Deliver routes an envelope to local subscribers. It validates the
	// envelope, because the transport's medium is not necessarily trusted,
	// and returns an error for one it rejected.
	Deliver(Envelope) error
	// Gap reports that envelopes may have been lost (for example the
	// listening connection dropped and was re-established). Subscribers are
	// told to resync.
	Gap()
}

// Transport shares events between replicas. Delivery is best-effort and
// at-most-once: a transport that cannot reach its peers reports Gap when it
// recovers instead of replaying.
type Transport interface {
	// MaxPayloadBytes is the largest event payload the transport can carry.
	// The broker enforces it on every replica's Publish.
	MaxPayloadBytes() int
	// Publish forwards an envelope to the other replicas.
	Publish(ctx context.Context, envelope Envelope) error
	// Run receives envelopes until ctx ends, reconnecting as needed. It
	// returns nil on ctx cancellation.
	Run(ctx context.Context, receiver Receiver) error
}
