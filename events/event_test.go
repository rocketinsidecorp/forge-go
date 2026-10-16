package events_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/events"
)

func TestNewCreatesValidEventAndCopiesPayload(t *testing.T) {
	payload := json.RawMessage(`{"id":"task-1"}`)
	event, err := events.New("task.created", 1, payload)
	if err != nil {
		t.Fatal(err)
	}
	payload[2] = 'x'
	if event.Payload[2] != 'i' {
		t.Fatalf("event payload aliases caller memory: %s", event.Payload)
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if event.ID.Version() != 7 || event.Type != "task.created" || event.Version != 1 || event.OccurredAt.Location() != time.UTC {
		t.Fatalf("unexpected event envelope: %+v", event)
	}
}

func TestEventValidateRejectsInvalidEnvelope(t *testing.T) {
	valid, err := events.New("task.created", 1, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*events.Event)
	}{
		{name: "invalid ID", mutate: func(event *events.Event) { event.ID = [16]byte{} }},
		{name: "invalid type", mutate: func(event *events.Event) { event.Type = "Task Created" }},
		{name: "type too long", mutate: func(event *events.Event) { event.Type = "a" + strings.Repeat("b", 200) }},
		{name: "zero version", mutate: func(event *events.Event) { event.Version = 0 }},
		{name: "version too large", mutate: func(event *events.Event) { event.Version = events.MaxVersion + 1 }},
		{name: "missing time", mutate: func(event *events.Event) { event.OccurredAt = time.Time{} }},
		{name: "invalid JSON", mutate: func(event *events.Event) { event.Payload = json.RawMessage(`{`) }},
		{name: "empty payload", mutate: func(event *events.Event) { event.Payload = nil }},
		{name: "oversized payload", mutate: func(event *events.Event) {
			event.Payload = json.RawMessage(`"` + strings.Repeat("x", events.MaxPayloadBytes) + `"`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			test.mutate(&event)
			if err := event.Validate(); err == nil {
				t.Fatal("Validate() succeeded for invalid event")
			}
		})
	}
}
