package realtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fabriciobonjorno/forge-go/tenancy"
	"github.com/fabriciobonjorno/forge-go/uuid"
)

const (
	// SyntheticPrefix reserves event types for events the broker itself
	// produces. Publishers cannot use it.
	SyntheticPrefix = "forge."

	// EventSubscribed acknowledges a subscription; it precedes any replayed
	// events for that topic.
	EventSubscribed = SyntheticPrefix + "subscribed"
	// EventResync tells a client that it may have missed events on a topic
	// (the retained replay window did not cover its Last-Event-ID, a replica
	// restarted, or the cross-replica link was interrupted). The client should
	// reload the state the topic describes.
	EventResync = SyntheticPrefix + "resync"
)

var (
	topicPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
)

// ValidTopic reports whether name is a legal topic name:
// ^[a-z][a-z0-9_.:-]{0,127}$.
func ValidTopic(name string) bool { return topicPattern.MatchString(name) }

// Event is one message published to a topic. Events handed to subscribers are
// shared between them and must not be modified.
type Event struct {
	// ID is a UUIDv7 assigned when the event is published. Synthetic events
	// (EventSubscribed, EventResync) have a zero ID and are never replayed.
	ID uuid.UUID `json:"id"`
	// Topic is the logical topic, without the tenant that scopes it.
	Topic string `json:"topic"`
	// Type is the application event name, such as "message.created".
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	// Data is the JSON payload.
	Data json.RawMessage `json:"data"`
}

// Synthetic reports whether the broker produced the event.
func (e Event) Synthetic() bool { return strings.HasPrefix(e.Type, SyntheticPrefix) }

// Frame is the JSON message clients receive, identical over SSE (as the
// data field) and WebSocket. Type is one of "event", "subscribed",
// "unsubscribed", "resync", "pong" or "error".
type Frame struct {
	Type    string          `json:"type"`
	Topic   string          `json:"topic,omitempty"`
	Event   string          `json:"event,omitempty"`
	ID      string          `json:"id,omitempty"`
	Time    string          `json:"time,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Ref     string          `json:"ref,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// FrameFor converts a delivered event into the frame sent to clients.
func FrameFor(e Event) Frame {
	frame := Frame{Topic: e.Topic}
	switch e.Type {
	case EventSubscribed:
		frame.Type = "subscribed"
		return frame
	case EventResync:
		frame.Type = "resync"
		return frame
	}
	frame.Type = "event"
	frame.Event = e.Type
	if e.ID != (uuid.UUID{}) {
		frame.ID = e.ID.String()
	}
	frame.Time = e.Time.UTC().Format(time.RFC3339Nano)
	frame.Data = e.Data
	return frame
}

// scope says whose topic namespace an event lives in: one tenant's or the
// global one. The zero scope is invalid.
type scope struct {
	tenant uuid.UUID
	global bool
}

const globalScope = "global"

func tenantScope(t tenancy.Tenant) scope { return scope{tenant: t.ID} }

// key is the routing key. The "g/" and "t/" prefixes cannot collide, and a
// tenant UUID has a fixed length, so no topic name can reach another scope.
func (s scope) key(topic string) string {
	if s.global {
		return "g/" + topic
	}
	return "t/" + s.tenant.String() + "/" + topic
}

func (s scope) String() string {
	if s.global {
		return globalScope
	}
	return s.tenant.String()
}

func parseScope(value string) (scope, error) {
	if value == globalScope {
		return scope{global: true}, nil
	}
	id, err := uuid.Parse(value)
	if err != nil || id.Version() != 7 || id.Variant() != 2 {
		return scope{}, fmt.Errorf("scope %q is neither %q nor a tenant UUIDv7", value, globalScope)
	}
	return scope{tenant: id}, nil
}
