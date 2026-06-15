// Package gcodelog keeps a bounded in-memory log of the gcode/console traffic
// flowing through the proxy — command lines sent to the machine and the output
// it returns — and fans each line out to subscribers (the web UI's SSE stream).
//
// Both conversations land here: the official controller's traffic observed by
// the relay, and lines injected via the proxy's own API. The log is
// deliberately not persisted; it is operational visibility, not state, and a
// restart starting empty is correct.
package gcodelog

import (
	"strings"
	"sync"
	"time"
)

// Directions of a logged line relative to the machine.
const (
	DirSend = "send" // toward the machine (gcode/console input)
	DirRecv = "recv" // from the machine (command output)
)

// Sources attribute a line to the conversation it belongs to.
const (
	SourceController = "controller" // official controller traffic, via the relay
	SourceAPI        = "api"        // proxy API (web console) traffic
	SourceJog        = "jog"        // proxy-generated gamepad jog motion/control
)

// Line is one logged gcode I/O line.
type Line struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Dir    string    `json:"dir"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
}

// Log is a ring of the most recent lines plus a subscriber fan-out.
type Log struct {
	mu      sync.Mutex
	cap     int
	ring    []Line
	nextSeq int64
	subs    map[int]chan Line
	nextSub int
}

// New creates a log keeping the most recent capacity lines.
func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 256
	}
	return &Log{cap: capacity, subs: map[int]chan Line{}, nextSeq: 1}
}

// Append records text, splitting multi-line payloads (machine output often
// carries several "\r\n"-terminated lines per frame) into one entry per line.
// Blank lines are dropped.
func (l *Log) Append(dir, source, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range strings.Split(text, "\n") {
		t = strings.TrimRight(t, "\r")
		if strings.TrimSpace(t) == "" {
			continue
		}
		ln := Line{Seq: l.nextSeq, Time: time.Now(), Dir: dir, Source: source, Text: t}
		l.nextSeq++
		l.ring = append(l.ring, ln)
		if len(l.ring) > l.cap {
			l.ring = append(l.ring[:0], l.ring[len(l.ring)-l.cap:]...)
		}
		for _, ch := range l.subs {
			select {
			case ch <- ln:
			default:
				// Drop for a slow subscriber; it can resync from Recent.
			}
		}
	}
}

// Recent returns a copy of the retained lines, oldest first. Never nil, so it
// serializes as a JSON array.
func (l *Log) Recent() []Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Line, len(l.ring))
	copy(out, l.ring)
	return out
}

// Replace imports a retained log snapshot. It does not replay imported lines to
// subscribers; clients can re-fetch /api/gcode/log or reconnect SSE.
func (l *Log) Replace(lines []Line) {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := 0
	if len(lines) > l.cap {
		start = len(lines) - l.cap
	}
	l.ring = append(l.ring[:0], lines[start:]...)
	maxSeq := int64(0)
	for i := range l.ring {
		if l.ring[i].Seq > maxSeq {
			maxSeq = l.ring[i].Seq
		}
	}
	l.nextSeq = maxSeq + 1
	if l.nextSeq <= 0 {
		l.nextSeq = 1
	}
}

// Subscribe returns a channel of new lines and an unsubscribe func. Lines
// already retained are not replayed; combine with Recent (Seq is globally
// increasing, so duplicates are detectable).
func (l *Log) Subscribe() (<-chan Line, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextSub
	l.nextSub++
	ch := make(chan Line, 256)
	l.subs[id] = ch
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if c, ok := l.subs[id]; ok {
			delete(l.subs, id)
			close(c)
		}
	}
}
