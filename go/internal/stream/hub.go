// Package stream implements the server's live event fan-out.
//
// Every session has one hub. Producers publish events to it; every connected
// SSE client receives a copy. The hub owns three things the HTTP layer must not
// reimplement:
//
//   - ordering: each subscriber sees events in publish order;
//   - slow-consumer isolation: a client that stops reading is dropped rather
//     than blocking the publisher, which is what keeps one stalled browser tab
//     from freezing a session;
//   - the wire encoding: “event: <type>\ndata: <json>\n\n“, matching the
//     Python serializer byte for byte.
//
// Sequence numbers are deliberately absent for session-scoped events: the
// Python server only numbers events that flow through the per-response
// serializer, and a session-scoped event (presence, heartbeat, status change)
// arrives with “sequence_number: null“. Numbering them here would produce a
// field the client's de-duplication would then have to ignore.
package stream

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Event is one frame on a session's stream.
type Event struct {
	// Type is the SSE event name and the payload's ``type`` discriminator,
	// e.g. "session.heartbeat". The two are always the same on this wire.
	Type string
	// Payload is the JSON body. The caller supplies the fields; the hub adds
	// nothing, so the shape stays exactly what the document declares.
	Payload map[string]any
}

// Encode renders the frame in SSE wire format.
//
// The trailing blank line terminates the frame; without it a client buffers the
// event until the next one arrives.
func (e Event) Encode() []byte {
	var body strings.Builder
	body.WriteString("event: ")
	body.WriteString(e.Type)
	body.WriteString("\ndata: ")
	encoded, err := json.Marshal(e.Payload)
	if err != nil {
		// A payload that cannot be marshalled is a producer bug; emitting a
		// minimal valid frame keeps the stream alive for everyone else rather
		// than tearing it down.
		encoded = []byte(`{"type":"` + e.Type + `","sequence_number":null}`)
	}
	body.Write(encoded)
	body.WriteString("\n\n")
	return []byte(body.String())
}

// subscriber is one connected client.
type subscriber struct {
	ch chan Event
	// dropped reports whether this subscriber fell behind. It exists so the
	// stream handler can tell the client to reconnect rather than leaving it
	// silently missing events.
	dropped bool
	mu      sync.Mutex
}

// Hub fans one session's events out to its subscribers.
//
// The zero value is not usable; call NewHub.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
	// buffer is the per-subscriber queue depth. A publisher never blocks: if a
	// subscriber's queue is full it is marked dropped and removed, because a
	// client that cannot keep up will resynchronise from a snapshot rather than
	// from a truncated stream.
	buffer int
}

// NewHub returns a hub with the given per-subscriber buffer depth.
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = 64
	}
	return &Hub{
		subscribers: make(map[*subscriber]struct{}),
		buffer:      buffer,
	}
}

// Subscribe registers a listener and returns its channel plus an unsubscribe
// function. The caller must invoke unsubscribe (typically deferred) or the hub
// keeps sending until the process exits.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, h.buffer)}
	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if _, ok := h.subscribers[sub]; ok {
			delete(h.subscribers, sub)
			close(sub.ch)
		}
		h.mu.Unlock()
	}
	return sub.ch, unsubscribe
}

// Publish delivers an event to every subscriber.
//
// Delivery is non-blocking: a subscriber whose buffer is full is dropped rather
// than stalling the publisher. That is the right trade here because a slow
// client has already fallen behind and will resynchronise from the session
// snapshot on reconnect, while blocking would delay every other client.
func (h *Hub) Publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subscribers {
		select {
		case sub.ch <- event:
		default:
			sub.mu.Lock()
			sub.dropped = true
			sub.mu.Unlock()
			delete(h.subscribers, sub)
			close(sub.ch)
		}
	}
}

// SubscriberCount reports how many clients are attached, which the presence
// event needs.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Registry owns one hub per session.
type Registry struct {
	mu   sync.Mutex
	hubs map[string]*Hub
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{hubs: make(map[string]*Hub)}
}

// ForSession returns the hub for a session, creating it on first use.
func (r *Registry) ForSession(sessionID string) *Hub {
	r.mu.Lock()
	defer r.mu.Unlock()
	hub, ok := r.hubs[sessionID]
	if !ok {
		hub = NewHub(64)
		r.hubs[sessionID] = hub
	}
	return hub
}

// Drop releases a session's hub once its last client leaves.
//
// Keeping an idle hub would leak one goroutine-free but still allocated map
// entry per session ever streamed, which on a long-lived server is unbounded.
func (r *Registry) Drop(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hubs, sessionID)
}

// HeartbeatInterval is how often an idle stream emits a keepalive.
//
// The Python server sends one on the same cadence; matching it means a proxy's
// idle timeout behaves identically against either build.
const HeartbeatInterval = 20 * time.Second

// SessionHeartbeat builds the keepalive frame.
//
// “server_time“ is absent rather than zero: the Python server leaves it null
// for a session-scoped heartbeat (the field is populated only on response
// events, where it timestamps a token delta).
func SessionHeartbeat(sessionID string) Event {
	return Event{
		Type: "session.heartbeat",
		Payload: map[string]any{
			"type":            "session.heartbeat",
			"conversation_id": sessionID,
			"sequence_number": nil,
			"server_time":     nil,
		},
	}
}

// SessionPresence builds the co-viewer frame.
//
// viewers is always present and sorted by the caller: the client diffs it
// against its previous value, so an absent key would read as "no change".
func SessionPresence(sessionID string, viewers []map[string]any) Event {
	if viewers == nil {
		viewers = []map[string]any{}
	}
	return Event{
		Type: "session.presence",
		Payload: map[string]any{
			"type":            "session.presence",
			"conversation_id": sessionID,
			"sequence_number": nil,
			"viewers":         viewers,
		},
	}
}

// SessionStatus builds a status transition frame.
func SessionStatus(sessionID, status string) Event {
	return Event{
		Type: "session.status",
		Payload: map[string]any{
			"type":            "session.status",
			"conversation_id": sessionID,
			"sequence_number": nil,
			"status":          status,
		},
	}
}
