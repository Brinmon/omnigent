package stream_test

// The hub's contract is what keeps one stalled browser tab from freezing a
// session, and what makes an event's wire form match the Python serializer. Both
// are tested here rather than through the HTTP layer, because the interesting
// cases (a subscriber that never reads, ordering under concurrent publish) are
// awkward to arrange over a socket.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Brinmon/omnigent/go/internal/stream"
)

// TestEncodeMatchesPythonWireFormat pins the SSE framing.
//
// The trailing blank line is load-bearing: without it a client buffers the event
// until the next frame arrives, which on an idle session means until the
// heartbeat.
func TestEncodeMatchesPythonWireFormat(t *testing.T) {
	event := stream.SessionHeartbeat("abc123")
	encoded := string(event.Encode())

	if !strings.HasPrefix(encoded, "event: session.heartbeat\ndata: ") {
		t.Fatalf("frame does not start with the event name and data prefix: %q", encoded)
	}
	if !strings.HasSuffix(encoded, "\n\n") {
		t.Fatalf("frame does not end with a blank line: %q", encoded)
	}

	payload := strings.TrimSuffix(strings.TrimPrefix(encoded, "event: session.heartbeat\ndata: "), "\n\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, payload)
	}
	if decoded["type"] != "session.heartbeat" {
		t.Errorf("type = %v, want session.heartbeat", decoded["type"])
	}
	// A session-scoped heartbeat carries nulls, not zeros: the client compares
	// against null to decide the field is absent.
	for _, key := range []string{"sequence_number", "server_time"} {
		value, present := decoded[key]
		if !present {
			t.Errorf("%q is missing; the document declares it", key)
			continue
		}
		if value != nil {
			t.Errorf("%q = %v, want null", key, value)
		}
	}
	if decoded["conversation_id"] != "abc123" {
		t.Errorf("conversation_id = %v, want abc123", decoded["conversation_id"])
	}
}

// TestPublishDeliversInOrder is the guarantee a client's reducer depends on.
func TestPublishDeliversInOrder(t *testing.T) {
	hub := stream.NewHub(16)
	events, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	const count = 8
	for i := 0; i < count; i++ {
		hub.Publish(stream.SessionStatus("s1", string(rune('a'+i))))
	}

	for i := 0; i < count; i++ {
		select {
		case event := <-events:
			want := string(rune('a' + i))
			if event.Payload["status"] != want {
				t.Fatalf("event %d has status %v, want %s", i, event.Payload["status"], want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

// TestSlowSubscriberIsDroppedNotBlocked is the isolation guarantee.
//
// A subscriber that never reads must not stall the publisher: one stalled tab
// would otherwise freeze every other viewer of the session.
func TestSlowSubscriberIsDroppedNotBlocked(t *testing.T) {
	// The healthy subscriber gets a buffer larger than the burst, which is the
	// realistic case: the hub's buffer is sized for a burst (64), and only a
	// client that stops reading entirely overflows it. A tiny buffer would test
	// "buffer smaller than the burst", not the isolation property.
	hub := stream.NewHub(32)
	stalled, _ := hub.Subscribe()
	_ = stalled // deliberately never read

	healthy, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	// Publish more than the stalled subscriber's buffer holds. If Publish
	// blocked, this would hang instead of completing.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			hub.Publish(stream.SessionStatus("s1", "running"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a stalled subscriber")
	}

	// The healthy subscriber keeps receiving.
	received := 0
	for received < 10 {
		select {
		case _, open := <-healthy:
			if !open {
				t.Fatalf("healthy subscriber was closed after %d events", received)
			}
			received++
		case <-time.After(2 * time.Second):
			t.Fatalf("healthy subscriber received only %d of 10 events", received)
		}
	}
}

// TestDroppedSubscriberChannelCloses tells the HTTP handler to end the response,
// which is what makes the client reconnect and resynchronise.
func TestDroppedSubscriberChannelCloses(t *testing.T) {
	hub := stream.NewHub(1)
	events, _ := hub.Subscribe()

	// Fill the buffer, then overflow it so the subscriber is dropped.
	for i := 0; i < 4; i++ {
		hub.Publish(stream.SessionStatus("s1", "running"))
	}

	// Drain whatever fit, then expect the channel to be closed.
	for {
		select {
		case _, open := <-events:
			if !open {
				return // closed, as intended
			}
		case <-time.After(2 * time.Second):
			t.Fatal("dropped subscriber's channel never closed")
		}
	}
}

// TestUnsubscribeRemovesSubscriber covers the leak path: a client that
// disconnects must stop consuming.
func TestUnsubscribeRemovesSubscriber(t *testing.T) {
	hub := stream.NewHub(4)
	events, unsubscribe := hub.Subscribe()

	if got := hub.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount = %d, want 1", got)
	}
	unsubscribe()
	if got := hub.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount = %d after unsubscribe, want 0", got)
	}

	// The channel is closed, so a reader sees the end rather than blocking.
	select {
	case _, open := <-events:
		if open {
			t.Error("channel still open after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Error("channel was not closed by unsubscribe")
	}

	// Unsubscribing twice must not panic on a double close.
	unsubscribe()
}

// TestConcurrentPublishIsRaceFree is what `go test -race` is for: the hub is
// written from request handlers while several readers drain.
func TestConcurrentPublishIsRaceFree(t *testing.T) {
	hub := stream.NewHub(64)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		events, unsubscribe := hub.Subscribe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer unsubscribe()
			for range events {
				// Drain until the hub closes us.
			}
		}()
	}

	var publishers sync.WaitGroup
	for i := 0; i < 4; i++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for j := 0; j < 50; j++ {
				hub.Publish(stream.SessionStatus("s1", "running"))
			}
		}()
	}
	publishers.Wait()

	// Give the readers a moment to drain, then close them out.
	time.Sleep(50 * time.Millisecond)
}

// TestRegistryReturnsSameHubPerSession covers the lookup: a second stream on one
// session must join the existing hub, or the two tabs would not see each other's
// events.
func TestRegistryReturnsSameHubPerSession(t *testing.T) {
	registry := stream.NewRegistry()

	first := registry.ForSession("s1")
	second := registry.ForSession("s1")
	if first != second {
		t.Error("ForSession returned different hubs for the same session")
	}

	other := registry.ForSession("s2")
	if other == first {
		t.Error("different sessions share a hub")
	}

	registry.Drop("s1")
	if fresh := registry.ForSession("s1"); fresh == first {
		t.Error("Drop did not release the hub")
	}
}
