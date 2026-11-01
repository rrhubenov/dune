// Package events fans out change notifications to subscribers (the web UI's
// WebSocket). Events say what changed, not how: clients refetch the named
// collection. Slow subscribers miss events rather than block publishers.
package events

import "sync"

// Topics.
const (
	VMs       = "vms"
	Snapshots = "snapshots"
	Storage   = "storage"
	Images    = "images"
	Ports     = "ports"
)

type Event struct {
	Topic string `json:"topic"`
}

type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Publish is safe on a nil Hub.
func (h *Hub) Publish(topics ...string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		for _, t := range topics {
			select {
			case ch <- Event{Topic: t}:
			default:
			}
		}
	}
}

// Subscribe returns a channel of events and a function to unsubscribe.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
