// Package timeline records migration phase timestamps against the host's
// CLOCK_MONOTONIC.
//
// Go's time.Now() carries a monotonic reading, but it is relative to process
// start and therefore meaningless across processes. The two migration agents
// run in separate containers that share one kernel, so raw CLOCK_MONOTONIC
// values ARE directly comparable between them — that is what lets us compute
// "blackout = resume timestamp on the target minus pause timestamp on the
// source" without clock synchronization. On physically separate hosts this
// shortcut disappears and you would use PTP; the client-observed probe gap
// (which needs no shared clock) remains the ground truth either way.
package timeline

import (
	"sync"

	"golang.org/x/sys/unix"
)

// Event is a named point on the shared monotonic clock.
type Event struct {
	Name string `json:"name"`
	NS   int64  `json:"ns"` // CLOCK_MONOTONIC, nanoseconds
}

// Now returns the shared monotonic clock reading in nanoseconds.
func Now() int64 {
	var ts unix.Timespec
	// CLOCK_MONOTONIC cannot fail with a valid timespec pointer.
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano()
}

// Recorder accumulates events; safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Mark records name at the current monotonic time and returns the reading.
func (r *Recorder) Mark(name string) int64 {
	ns := Now()
	r.MarkAt(name, ns)
	return ns
}

// MarkAt records an event that was timestamped elsewhere (e.g. on the peer
// agent, whose CLOCK_MONOTONIC is the same clock).
func (r *Recorder) MarkAt(name string, ns int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, Event{Name: name, NS: ns})
}

// Events returns a copy of the recorded events in insertion order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Between returns the duration in nanoseconds between the first occurrence of
// two named events, or false if either is missing.
func (r *Recorder) Between(from, to string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var fromNS, toNS int64
	var haveFrom, haveTo bool
	for _, e := range r.events {
		if !haveFrom && e.Name == from {
			fromNS, haveFrom = e.NS, true
		}
		if !haveTo && e.Name == to {
			toNS, haveTo = e.NS, true
		}
	}
	if !haveFrom || !haveTo {
		return 0, false
	}
	return toNS - fromNS, true
}
