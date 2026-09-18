package bridge

import (
	"sync"
	"time"
)

// throttle rate-limits a stream of integer readings down to something a
// broker can live with.
//
// A moving desk pushes a ReferenceOutput notification every few tens of
// milliseconds. Publishing each one is wasteful and, with retain on,
// pointlessly rewrites the retained message dozens of times per move. So
// updates are coalesced: a value is published at most once per min
// interval, unchanged values are dropped entirely, and Set(v, force=true)
// (used when the desk reports speed 0, i.e. it has stopped) always
// publishes so the final resting position is never left in the pending
// slot.
type throttle struct {
	min     time.Duration
	publish func(int)

	mu         sync.Mutex
	pending    int
	hasPending bool
	last       int
	hasLast    bool
	lastAt     time.Time
	now        func() time.Time // overridable in tests
}

func newThrottle(min time.Duration, publish func(int)) *throttle {
	return &throttle{min: min, publish: publish, now: time.Now}
}

// Set offers a new reading. force publishes immediately (subject only to
// the value having changed).
func (t *throttle) Set(v int, force bool) {
	t.mu.Lock()
	if t.hasLast && t.last == v {
		// Nothing new to say; drop any pending duplicate too.
		t.hasPending = false
		t.mu.Unlock()
		return
	}
	now := t.now()
	if force || !t.hasLast || now.Sub(t.lastAt) >= t.min {
		t.last, t.hasLast, t.lastAt = v, true, now
		t.hasPending = false
		t.mu.Unlock()
		t.publish(v)
		return
	}
	t.pending, t.hasPending = v, true
	t.mu.Unlock()
}

// Flush publishes a pending value if the min interval has elapsed. Call it
// from a ticker so a stream that stops mid-interval still settles on its
// last value.
func (t *throttle) Flush() {
	t.mu.Lock()
	if !t.hasPending {
		t.mu.Unlock()
		return
	}
	now := t.now()
	if t.hasLast && now.Sub(t.lastAt) < t.min {
		t.mu.Unlock()
		return
	}
	v := t.pending
	t.pending, t.hasPending = 0, false
	t.last, t.hasLast, t.lastAt = v, true, now
	t.mu.Unlock()
	t.publish(v)
}

// Reset forgets the last published value, so the next reading is always
// published. Used on reconnect, where re-announcing the current height
// even if it has not changed is the point.
func (t *throttle) Reset() {
	t.mu.Lock()
	t.hasLast, t.hasPending = false, false
	t.mu.Unlock()
}
