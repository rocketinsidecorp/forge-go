package realtime

import (
	"time"

	"github.com/fabriciobonjorno/forge-go/uuid"
)

// replayStore retains recent events so a reconnecting client can resume from
// its Last-Event-ID. It is one process-wide FIFO bounded by event count,
// payload bytes and age, so the memory it can hold is fixed regardless of how
// many topics exist. It is not safe for concurrent use: the broker's mutex
// guards it, which also makes "snapshot the replay window and register the
// subscriber" atomic with respect to publishing.
type replayStore struct {
	maxEvents int
	maxBytes  int
	ttl       time.Duration

	entries []replayEntry
	head    int
	bytes   int

	// floor is the newest ID that may have left the window (evicted, expired,
	// or lost while the cross-replica link was down). A client whose
	// Last-Event-ID is older than the floor may have missed events.
	floor uuid.UUID
}

type replayEntry struct {
	key   string
	event Event
	size  int
	at    time.Time
}

func newReplayStore(maxEvents, maxBytes int, ttl time.Duration, startFloor uuid.UUID) *replayStore {
	return &replayStore{maxEvents: maxEvents, maxBytes: maxBytes, ttl: ttl, floor: startFloor}
}

func (s *replayStore) enabled() bool { return s.maxEvents > 0 }

func (s *replayStore) len() int { return len(s.entries) - s.head }

func (s *replayStore) add(key string, event Event, now time.Time) {
	if !s.enabled() {
		return
	}
	size := len(event.Data) + len(event.Topic) + len(event.Type) + 64
	if size > s.maxBytes {
		// An event larger than the whole window can never be retained; it
		// still advances the floor so resumes report the gap.
		s.raiseFloor(event.ID)
		return
	}
	s.entries = append(s.entries, replayEntry{key: key, event: event, size: size, at: now})
	s.bytes += size
	s.prune(now)
}

func (s *replayStore) prune(now time.Time) {
	for s.len() > 0 {
		oldest := s.entries[s.head]
		if s.len() <= s.maxEvents && s.bytes <= s.maxBytes && now.Sub(oldest.at) <= s.ttl {
			break
		}
		s.raiseFloor(oldest.event.ID)
		s.bytes -= oldest.size
		s.entries[s.head] = replayEntry{} // release the payload
		s.head++
	}
	// Compact once the dead prefix dominates, keeping append amortized O(1).
	if s.head > 1024 && s.head*2 >= len(s.entries) {
		s.entries = append(s.entries[:0], s.entries[s.head:]...)
		s.head = 0
	}
}

func (s *replayStore) raiseFloor(id uuid.UUID) {
	if id.Compare(s.floor) > 0 {
		s.floor = id
	}
}

// markGap records that events may have been lost at the given moment.
func (s *replayStore) markGap(id uuid.UUID) { s.raiseFloor(id) }

// since returns, oldest first, the retained events for key newer than after.
// gap is true when events between after and the first returned event may no
// longer be retained. At most limit events are returned (the newest); any cut
// also sets gap.
func (s *replayStore) since(key string, after uuid.UUID, now time.Time, limit int) (events []Event, gap bool) {
	if !s.enabled() {
		return nil, true
	}
	s.prune(now)
	gap = after.Compare(s.floor) < 0
	for _, entry := range s.entries[s.head:] {
		if entry.key == key && entry.event.ID.Compare(after) > 0 {
			events = append(events, entry.event)
		}
	}
	if limit >= 0 && len(events) > limit {
		events = events[len(events)-limit:]
		gap = true
	}
	return events, gap
}
