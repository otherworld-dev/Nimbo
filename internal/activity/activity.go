// Package activity records a live, bounded history of sync operations and
// unresolved errors so a UI can show what happened and what needs attention. It
// is toolkit-agnostic (no GUI dependency) and safe for concurrent use.
package activity

import (
	"sync"
	"time"
)

// maxEvents bounds the in-memory history (oldest dropped first).
const maxEvents = 500

// Event is a single recorded sync operation.
type Event struct {
	Time  time.Time
	Local string // the sync pair's local dir (which folder this belongs to)
	Path  string // pair-relative path
	Kind  string // download, upload, move, delete-local, … (engine.ActionKind.String())
	Err   string // empty on success
}

// OK reports whether the event represents a successful operation.
func (e Event) OK() bool { return e.Err == "" }

// Recorder holds recent events and the set of currently-unresolved errors, and
// notifies subscribers of new events.
type Recorder struct {
	mu     sync.Mutex
	events []Event          // ring buffer, newest last
	errs   map[string]Event // key = Local|Path, only unresolved failures
	subs   []chan Event
}

// New creates an empty Recorder.
func New() *Recorder {
	return &Recorder{errs: make(map[string]Event)}
}

// Add records an event. A success clears any prior unresolved error for the same
// path; a failure records one. Subscribers are notified (non-blocking).
func (r *Recorder) Add(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	if len(r.events) > maxEvents {
		r.events = r.events[len(r.events)-maxEvents:]
	}
	key := e.Local + "|" + e.Path
	if e.OK() {
		delete(r.errs, key) // the path now synced cleanly — error resolved
	} else {
		r.errs[key] = e
	}
	subs := append([]chan Event(nil), r.subs...)
	r.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default: // slow consumer: drop rather than block the sync path
		}
	}
}

// Recent returns a copy of the recorded events, newest first.
func (r *Recorder) Recent() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	for i, e := range r.events {
		out[len(r.events)-1-i] = e
	}
	return out
}

// Errors returns the currently-unresolved error events, newest first.
func (r *Recorder) Errors() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, 0, len(r.errs))
	for _, e := range r.errs {
		out = append(out, e)
	}
	// newest first
	for i := 0; i < len(out)/2; i++ {
		j := len(out) - 1 - i
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Subscribe returns a channel that receives future events. The buffer absorbs
// short bursts; events are dropped for a consumer that can't keep up.
func (r *Recorder) Subscribe() <-chan Event {
	ch := make(chan Event, 64)
	r.mu.Lock()
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	return ch
}
