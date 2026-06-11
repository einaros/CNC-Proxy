// Package machine models the CNC's reported run state, which gates all file
// operations: the firmware only accepts file commands when Idle.
package machine

import (
	"strings"
	"sync"
	"time"
)

// State is the machine's run state, taken from the first field of a status
// report like "<Idle|MPos:...|...>".
type State string

const (
	Unknown State = ""
	Idle    State = "Idle"
	Run     State = "Run"
	Hold    State = "Hold"
	Alarm   State = "Alarm"
	Sleep   State = "Sleep"
	Home    State = "Home"
)

// CanRunFileOps reports whether file operations are safe to execute. Only Idle
// qualifies; anything else (running a job, paused, alarmed, unknown) must wait.
func (s State) CanRunFileOps() bool {
	return s == Idle
}

// ParseStatus extracts the run state from a status report payload. It accepts
// the report with or without the enclosing angle brackets and tolerates
// trailing whitespace. Returns Unknown if it doesn't look like a status report.
func ParseStatus(payload string) (State, bool) {
	s := strings.TrimSpace(payload)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	if s == "" {
		return Unknown, false
	}
	first := s
	if i := strings.IndexByte(s, '|'); i >= 0 {
		first = s[:i]
	}
	first = strings.TrimSpace(first)
	switch State(first) {
	case Idle, Run, Hold, Alarm, Sleep, Home:
		return State(first), true
	default:
		return Unknown, false
	}
}

// Tracker holds the latest observed machine state with the time it was seen.
// It is safe for concurrent use: the relay observer and the sync engine both
// read it, and observers from either mode write it.
type Tracker struct {
	mu        sync.RWMutex
	state     State
	updatedAt time.Time
	now       func() time.Time // injectable for tests
}

// NewTracker returns a Tracker using wall-clock time.
func NewTracker() *Tracker {
	return &Tracker{now: time.Now}
}

// Observe records a freshly parsed state.
func (t *Tracker) Observe(s State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	t.updatedAt = t.now()
}

// ObserveStatusPayload parses a status report and records it if valid. Returns
// true if the payload was a recognizable status report.
func (t *Tracker) ObserveStatusPayload(payload string) bool {
	s, ok := ParseStatus(payload)
	if !ok {
		return false
	}
	t.Observe(s)
	return true
}

// Snapshot returns the current state and how long ago it was observed.
func (t *Tracker) Snapshot() (State, time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.updatedAt.IsZero() {
		return Unknown, 0
	}
	return t.state, t.now().Sub(t.updatedAt)
}

// Fresh reports whether the state was observed within maxAge. A stale state
// (e.g. no status seen recently) should not be trusted to gate file ops.
func (t *Tracker) Fresh(maxAge time.Duration) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.updatedAt.IsZero() {
		return false
	}
	return t.now().Sub(t.updatedAt) <= maxAge
}
