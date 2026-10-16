package uuid_test

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestGeneratorNew(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, time.September, 26, 12, 30, 0, 123_000_000, time.UTC)
	generator, err := uuid.NewGenerator(func() time.Time { return fixed }, bytes.NewReader(make([]byte, 20)))
	if err != nil {
		t.Fatal(err)
	}
	first, err := generator.New()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.New()
	if err != nil {
		t.Fatal(err)
	}
	if first.Version() != 7 || first.Variant() != 2 {
		t.Fatalf("version=%d variant=%d", first.Version(), first.Variant())
	}
	if !first.Time().Equal(fixed.Truncate(time.Millisecond)) {
		t.Fatalf("time=%s want=%s", first.Time(), fixed.Truncate(time.Millisecond))
	}
	if first.Compare(second) >= 0 {
		t.Fatalf("UUIDs are not monotonic: %s >= %s", first, second)
	}
}

func TestGeneratorConcurrentUniqueness(t *testing.T) {
	t.Parallel()
	fixed := time.UnixMilli(1_800_000_000_000)
	generator, err := uuid.NewGenerator(func() time.Time { return fixed }, bytes.NewReader(make([]byte, 10)))
	if err != nil {
		t.Fatal(err)
	}
	const count = 500
	results := make(chan uuid.UUID, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			id, generationErr := generator.New()
			if generationErr != nil {
				t.Errorf("generate: %v", generationErr)
				return
			}
			results <- id
		}()
	}
	group.Wait()
	close(results)
	seen := make(map[uuid.UUID]struct{}, count)
	for id := range results {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate UUID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseAndSerialization(t *testing.T) {
	t.Parallel()
	id, err := uuid.Parse("01890f6e-9c00-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got := id.String(); got != "01890f6e-9c00-7000-8000-000000000001" {
		t.Fatalf("String()=%q", got)
	}
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var decoded uuid.UUID
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if id != decoded {
		t.Fatalf("JSON round trip: %s != %s", id, decoded)
	}
	value, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(string); !ok {
		t.Fatalf("driver value type=%T want string", value)
	}
	var scanned uuid.UUID
	if err := scanned.Scan(value); err != nil {
		t.Fatal(err)
	}
	if scanned != id {
		t.Fatalf("SQL round trip: %s != %s", scanned, id)
	}
}

func TestParseRejectsNonV7(t *testing.T) {
	t.Parallel()
	invalid := []string{"", "not-a-uuid", "550e8400-e29b-41d4-a716-446655440000", "01890f6e-9c00-7000-0000-000000000001"}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := uuid.Parse(value); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	f.Add("01890f6e-9c00-7000-8000-000000000001")
	f.Add("invalid")
	f.Fuzz(func(t *testing.T, value string) {
		id, err := uuid.Parse(value)
		if err == nil && id.String() != value {
			t.Fatalf("non-canonical round trip: %q -> %q", value, id.String())
		}
	})
}
