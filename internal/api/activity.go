package api

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// activityTracker counts live data-plane connections per Sandbox and remembers
// the last touch, so an Idle Stop never stops a Sandbox that is being used and
// a request shorter than one sweep still resets its clock (ADR-0015).
type activityTracker struct {
	mu   sync.Mutex
	live map[uuid.UUID]int
	last map[uuid.UUID]time.Time
}

func newActivityTracker() *activityTracker {
	return &activityTracker{live: make(map[uuid.UUID]int), last: make(map[uuid.UUID]time.Time)}
}

func (a *activityTracker) begin(id uuid.UUID) {
	a.mu.Lock()
	a.live[id]++
	a.last[id] = time.Now()
	a.mu.Unlock()
}

func (a *activityTracker) end(id uuid.UUID) {
	a.mu.Lock()
	if a.live[id] <= 1 {
		delete(a.live, id)
	} else {
		a.live[id]--
	}
	a.last[id] = time.Now()
	a.mu.Unlock()
}

// touch records activity that has no connection lifetime, such as an Exec
// being created.
func (a *activityTracker) touch(id uuid.UUID) {
	a.mu.Lock()
	a.last[id] = time.Now()
	a.mu.Unlock()
}

func (a *activityTracker) count(id uuid.UUID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live[id]
}

func (a *activityTracker) lastActivity(id uuid.UUID) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	last, ok := a.last[id]
	return last, ok
}
