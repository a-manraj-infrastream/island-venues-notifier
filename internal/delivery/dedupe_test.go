package delivery

import (
	"fmt"
	"sync"
	"testing"
)

func TestSeenSetLifecycle(t *testing.T) {
	s := newSeenSet(10)
	if got := s.begin("a"); got != claimed {
		t.Fatalf("first begin = %v, want claimed", got)
	}
	if got := s.begin("a"); got != inProgress {
		t.Fatalf("begin while in flight = %v, want inProgress", got)
	}
	s.finish("a", false)
	if got := s.begin("a"); got != claimed {
		t.Fatalf("begin after failure = %v, want claimed", got)
	}
	s.finish("a", true)
	if got := s.begin("a"); got != duplicate {
		t.Fatalf("begin after success = %v, want duplicate", got)
	}
}

func TestSeenSetIsBoundedAndEvictsOldest(t *testing.T) {
	s := newSeenSet(3)
	for i := range 10 {
		id := fmt.Sprintf("m%d", i)
		s.begin(id)
		s.finish(id, true)
		if s.len() > 3 {
			t.Fatalf("set grew to %d, capacity 3", s.len())
		}
	}
	for i := range 7 {
		if got := s.begin(fmt.Sprintf("m%d", i)); got != claimed {
			t.Errorf("m%d should have been evicted, got %v", i, got)
		}
	}
	for i := 7; i < 10; i++ {
		if got := s.begin(fmt.Sprintf("m%d", i)); got != duplicate {
			t.Errorf("m%d should still be remembered, got %v", i, got)
		}
	}
}

func TestSeenSetConcurrentClaimsAreExclusive(t *testing.T) {
	s := newSeenSet(100)
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.begin("same") == claimed {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Errorf("%d goroutines claimed the same id, want 1", winners)
	}
}

func TestSeenSetNonPositiveCapacity(t *testing.T) {
	s := newSeenSet(0)
	s.begin("a")
	s.finish("a", true)
	if s.begin("a") != duplicate {
		t.Error("capacity 0 should still remember the latest id")
	}
}
