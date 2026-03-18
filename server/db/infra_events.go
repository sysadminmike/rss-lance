package db

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// infraEvent represents a storage infrastructure event captured in-memory.
// These are always captured regardless of storage state and drained to the
// log tables when any write tier recovers.
type infraEvent struct {
	Timestamp time.Time
	Level     string // "warn" or "error"
	Message   string
}

// infraRing is a fixed-size ring buffer for infrastructure events.
// Thread-safe. When full, oldest entries are overwritten.
type infraRing struct {
	mu      sync.Mutex
	events  []infraEvent
	head    int  // next write position
	count   int  // number of valid entries
	cap     int  // ring capacity
}

// newInfraRing creates a ring buffer with the given capacity.
func newInfraRing(capacity int) *infraRing {
	if capacity <= 0 {
		capacity = 100
	}
	return &infraRing{
		events: make([]infraEvent, capacity),
		cap:    capacity,
	}
}

// add appends an infrastructure event to the ring.
func (r *infraRing) add(level, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events[r.head] = infraEvent{
		Timestamp: time.Now().UTC(),
		Level:     level,
		Message:   message,
	}
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
}

// drain returns all buffered events in chronological order and clears the ring.
// Returns LogEntry values ready for insertion into the log table.
func (r *infraRing) drain() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}

	entries := make([]LogEntry, 0, r.count)

	// Read from oldest to newest
	start := 0
	if r.count == r.cap {
		start = r.head // oldest entry is at head when full
	}
	for i := 0; i < r.count; i++ {
		idx := (start + i) % r.cap
		e := r.events[idx]
		now := e.Timestamp
		entries = append(entries, LogEntry{
			LogID:     infraUUID(),
			Timestamp: &now,
			Level:     e.Level,
			Category:  "storage_events",
			Message:   e.Message,
			CreatedAt: &now,
		})
	}

	// Clear ring
	r.head = 0
	r.count = 0

	return entries
}

// pending returns the number of buffered events.
func (r *infraRing) pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// infraUUID generates a v4 UUID string without external dependencies.
func infraUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
