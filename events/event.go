// Package events defines the portable envelope used for application events.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	// MaxPayloadBytes bounds serialized event payloads to 1 MiB.
	MaxPayloadBytes = 1 << 20
	// MaxVersion is the largest schema version that fits PostgreSQL integer.
	MaxVersion = 1<<31 - 1
)

var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,199}$`)

// Event is a versioned application event. Payloads are opaque JSON; producers
// and consumers own their schema and must avoid putting secrets in them.
type Event struct {
	ID         uuid.UUID
	Type       string
	Version    int
	OccurredAt time.Time
	Payload    json.RawMessage
}

// New creates an event with a UUIDv7 and the current UTC time. Payload is
// copied so later caller mutations cannot alter the returned event.
func New(eventType string, version int, payload json.RawMessage) (Event, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, fmt.Errorf("generate event ID: %w", err)
	}
	event := Event{
		ID:         id,
		Type:       eventType,
		Version:    version,
		OccurredAt: time.Now().UTC(),
		Payload:    append(json.RawMessage(nil), payload...),
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Validate checks the stable envelope fields and bounds the opaque payload.
func (event Event) Validate() error {
	if event.ID.Version() != 7 || event.ID.Variant() != 2 {
		return errors.New("event ID must be a UUIDv7")
	}
	if !eventTypePattern.MatchString(event.Type) {
		return errors.New("event type must be 3-200 lowercase letters, digits, '.', '_' or '-' starting with a letter")
	}
	if event.Version < 1 || event.Version > MaxVersion {
		return errors.New("event version must be a positive 32-bit integer")
	}
	if event.OccurredAt.IsZero() {
		return errors.New("event occurrence time is required")
	}
	if len(event.Payload) == 0 || len(event.Payload) > MaxPayloadBytes || !json.Valid(event.Payload) {
		return fmt.Errorf("event payload must be valid JSON between 1 byte and %d bytes", MaxPayloadBytes)
	}
	return nil
}
