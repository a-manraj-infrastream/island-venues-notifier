package delivery

import "sync"

// claim is the outcome of trying to start work on a message id.
type claim int

const (
	claimed    claim = iota // first time we see it: the caller owns the send
	duplicate               // already sent successfully: acknowledge, do nothing
	inProgress              // another delivery of the same id is being handled now
)

// seenSet remembers the ids of messages that were handled successfully, up to
// a fixed capacity, evicting the oldest first. It is per-instance and lost on
// restart: it filters Pub/Sub's at-least-once redeliveries that land on the
// same instance within a short window, it is not an exactly-once guarantee.
//
// Ids are only recorded after a successful send. A failed send releases the
// id so the retry Pub/Sub makes is processed again instead of being swallowed
// as a duplicate.
type seenSet struct {
	mu       sync.Mutex
	capacity int
	done     map[string]struct{}
	order    []string // ring buffer of ids in done, oldest at head
	head     int
	inflight map[string]struct{}
}

func newSeenSet(capacity int) *seenSet {
	if capacity < 1 {
		capacity = 1
	}
	return &seenSet{
		capacity: capacity,
		done:     make(map[string]struct{}, capacity),
		order:    make([]string, 0, capacity),
		inflight: make(map[string]struct{}),
	}
}

// begin tries to take ownership of id.
func (s *seenSet) begin(id string) claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.done[id]; ok {
		return duplicate
	}
	if _, ok := s.inflight[id]; ok {
		return inProgress
	}
	s.inflight[id] = struct{}{}
	return claimed
}

// finish releases ownership of id; when sent is true the id is remembered so
// later deliveries are recognised as duplicates.
func (s *seenSet) finish(id string, sent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
	if !sent {
		return
	}
	if _, ok := s.done[id]; ok {
		return
	}
	if len(s.order) < s.capacity {
		s.order = append(s.order, id)
	} else {
		delete(s.done, s.order[s.head])
		s.order[s.head] = id
		s.head = (s.head + 1) % s.capacity
	}
	s.done[id] = struct{}{}
}

// len reports how many completed ids are remembered.
func (s *seenSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.done)
}
