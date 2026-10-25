package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type unencodable struct{}

func (unencodable) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot encode") }

// roundTripBreaker encodes a field its own strict decoder rejects.
type roundTripBreaker struct{ Known string }

func (roundTripBreaker) MarshalJSON() ([]byte, error) { return []byte(`{"unknown_field":1}`), nil }
func (roundTripBreaker) UnmarshalJSON(data []byte) error {
	var probe struct{ Known string }
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&probe)
}

func registryFor[T any](t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := Register(reg, "k", func(context.Context, T) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterAndPrepareRefuseWhatCouldNotBeRunLater(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	handler := func(context.Context, struct{}) error { return nil }
	if err := Register(reg, "k", handler, nil); err == nil || !strings.Contains(err.Error(), "nil handler option") {
		t.Errorf("a nil handler option: %v", err)
	}
	if _, err := Prepare(registryFor[struct{}](t), "k", struct{}{}, WithPriority(MaxPriority+1)); err == nil || !strings.Contains(err.Error(), "priority must be between") {
		t.Errorf("an out-of-range priority: %v", err)
	}
	if job, err := Prepare(registryFor[struct{}](t), "k", struct{}{}, WithPriority(-MaxPriority)); err != nil || job.Priority != -MaxPriority {
		t.Errorf("the lowest priority: %+v, %v", job, err)
	}
	if _, err := Prepare(registryFor[unencodable](t), "k", unencodable{}); err == nil || !strings.Contains(err.Error(), "encode k payload") {
		t.Errorf("a payload that cannot be encoded: %v", err)
	}
	// A payload the worker would reject is refused at enqueue time, where the
	// programmer can fix it, not dead-lettered later.
	if _, err := Prepare(registryFor[roundTripBreaker](t), "k", roundTripBreaker{}); err == nil || !strings.Contains(err.Error(), "does not round-trip") {
		t.Errorf("a payload that does not round-trip: %v", err)
	}
	if _, err := Enqueue[struct{}](context.Background(), nil, registryFor[struct{}](t), "k", struct{}{}); err == nil || !strings.Contains(err.Error(), "inserter is required") {
		t.Errorf("Enqueue without an inserter: %v", err)
	}
}
