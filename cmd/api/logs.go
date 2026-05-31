package main

import (
	"sync"
	"time"
)

// logBroadcaster keeps a ring of recent log lines and fans new lines out to
// every SSE subscriber.
type logBroadcaster struct {
	mu      sync.Mutex
	ring    []string
	head    int // next write index
	full    bool
	clients map[chan string]struct{}
}

func newLogBroadcaster(capacity int) *logBroadcaster {
	if capacity <= 0 {
		capacity = 256
	}
	return &logBroadcaster{
		ring:    make([]string, capacity),
		clients: make(map[chan string]struct{}),
	}
}

// Emit records line and forwards to subscribers without blocking.
func (b *logBroadcaster) Emit(line string) {
	stamped := time.Now().UTC().Format("2006-01-02T15:04:05Z") + " " + line
	b.mu.Lock()
	b.ring[b.head] = stamped
	b.head = (b.head + 1) % len(b.ring)
	if b.head == 0 {
		b.full = true
	}
	clients := make([]chan string, 0, len(b.clients))
	for ch := range b.clients {
		clients = append(clients, ch)
	}
	b.mu.Unlock()
	for _, ch := range clients {
		select {
		case ch <- stamped:
		default:
			// slow consumer — drop.
		}
	}
}

// subscribe returns a new channel plus the current backlog (oldest first).
func (b *logBroadcaster) subscribe() (chan string, []string) {
	ch := make(chan string, 128)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[ch] = struct{}{}
	return ch, b.snapshotLocked()
}

func (b *logBroadcaster) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *logBroadcaster) snapshotLocked() []string {
	if !b.full {
		out := make([]string, b.head)
		copy(out, b.ring[:b.head])
		return out
	}
	out := make([]string, 0, len(b.ring))
	out = append(out, b.ring[b.head:]...)
	out = append(out, b.ring[:b.head]...)
	return out
}
