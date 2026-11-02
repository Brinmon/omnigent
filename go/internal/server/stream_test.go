package server_test

// P2 acceptance: a client can post a message, follow the session stream, and
// observe the turn's status edges and reply frames in order.
//
// The stream and the POST are tested together because they are two halves of one
// protocol: the POST acknowledges acceptance (202) while progress arrives on the
// SSE connection, so a test that only checked the response would prove nothing
// about what the UI renders.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseFrame is one parsed SSE frame.
type sseFrame struct {
	Event string
	Data  map[string]any
}

// openStream connects to a session's SSE stream and reads frames in a goroutine.
func openStream(t *testing.T, ts *httptest.Server, sessionID string) (<-chan sseFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, ts.URL+"/v1/sessions/"+sessionID+"/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("stream status %d, want 200", resp.StatusCode)
	}

	frames := make(chan sseFrame, 32)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		decoder := newSSEDecoder(resp.Body)
		for {
			frame, err := decoder.next()
			if err != nil {
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	stop := func() {
		cancel()
		resp.Body.Close()
	}
	t.Cleanup(stop)
	return frames, stop
}

// postEvent sends one event to a session.
func postEvent(t *testing.T, ts *httptest.Server, sessionID string, body string) int {
	t.Helper()
	status, _ := postEventBody(t, ts, sessionID, body)
	return status
}

// postEventBody also returns the response body, so a failing case can report the
// server's reason instead of only its status.
func postEventBody(t *testing.T, ts *httptest.Server, sessionID, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(
		ts.URL+"/v1/sessions/"+sessionID+"/events",
		"application/json",
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("POST events: %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(responseBody)
}

// awaitEvent waits for a frame whose type is one of the wanted names, returning
// it and every frame seen before it.
func awaitEvent(t *testing.T, frames <-chan sseFrame, timeout time.Duration, want ...string) (sseFrame, []sseFrame) {
	t.Helper()
	deadline := time.After(timeout)
	seen := make([]sseFrame, 0, 8)
	for {
		select {
		case frame, open := <-frames:
			if !open {
				t.Fatalf("stream closed while waiting for %v; saw %v", want, frameTypes(seen))
			}
			seen = append(seen, frame)
			for _, name := range want {
				if frame.Event == name {
					return frame, seen
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %v; saw %v", want, frameTypes(seen))
		}
	}
}

func frameTypes(frames []sseFrame) []string {
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frame.Event)
	}
	return types
}

// TestStreamStartsWithPresence covers the opening frame: a client that connects
// must learn the co-viewer state before anything else.
func TestStreamStartsWithPresence(t *testing.T) {
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)

	frames, _ := openStream(t, ts, ids[0])
	frame, _ := awaitEvent(t, frames, 10*time.Second, "session.presence")

	if frame.Data["conversation_id"] != ids[0] {
		t.Errorf("presence conversation_id = %v, want %s", frame.Data["conversation_id"], ids[0])
	}
	if _, present := frame.Data["viewers"]; !present {
		t.Error("presence frame has no viewers key; the client diffs it")
	}
}

// TestPostMessageProducesTurnEdges is B4: a message produces running -> idle
// with a completed response in between, in that order.
func TestPostMessageProducesTurnEdges(t *testing.T) {
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	frames, _ := openStream(t, ts, sessionID)
	// The stream's first frame is presence; consume it so the assertions below
	// are about the turn.
	awaitEvent(t, frames, 10*time.Second, "session.presence")

	// The reply text depends on which model answers, so the assertion is about
	// the frame sequence and that *some* text came back -- not about content.
	status := postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Reply with exactly the word PONG."}]}}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST events status = %d, want 202", status)
	}

	// A real turn round-trips to a provider, so the deadline is generous: the
	// point of the test is the frame sequence, not the latency.
	running, _ := awaitEvent(t, frames, 120*time.Second, "session.status")
	if running.Data["status"] != "running" {
		t.Fatalf("first status = %v, want running", running.Data["status"])
	}

	completed, seen := awaitEvent(t, frames, 120*time.Second, "response.completed")
	// A real turn streams deltas before the terminal frame; that is what the UI
	// renders progressively, so its absence is a regression even though the
	// final text would still arrive.
	sawDelta := false
	for _, frame := range seen {
		if frame.Event == "response.output_text.delta" {
			sawDelta = true
			break
		}
	}
	if !sawDelta {
		t.Errorf("no response.output_text.delta before completion; saw %v", frameTypes(seen))
	}
	if text, _ := completed.Data["text"].(string); strings.TrimSpace(text) == "" {
		t.Errorf("response.completed carries no text: %v", completed.Data)
	}
	if completed.Data["echo"] == true {
		t.Error("response.completed is an echo; P3 should drive a real turn")
	}

	idle, _ := awaitEvent(t, frames, 120*time.Second, "session.status")
	if idle.Data["status"] != "idle" {
		t.Fatalf("final status = %v, want idle", idle.Data["status"])
	}
}

// TestPostEventShape covers the request envelope.
//
// This is a parsing test: it asserts which bodies are accepted and which are
// rejected, and deliberately does not wait for the turns it starts. Each accepted
// message launches a real Pi child, so the cases that only mean to exercise the
// envelope use a *separate* session -- sharing one would leave every case after
// the first testing the "turn already running" refusal instead of its own shape.
func TestPostEventShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"single object", `{"type":"message","data":{"content":[{"type":"input_text","text":"hi"}]}}`, http.StatusAccepted},
		{"array of one", `[{"type":"message","data":{"content":[{"type":"input_text","text":"hi"}]}}]`, http.StatusAccepted},
		{"interrupt", `{"type":"interrupt","data":{}}`, http.StatusAccepted},
		{"empty array", `[]`, http.StatusBadRequest},
		{"not JSON", `not json`, http.StatusBadRequest},
		{"unknown type", `{"type":"nonsense","data":{}}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh server per case: each owns its own session and its own
			// turn slot, so no case can be affected by another's running turn.
			ts := newTestServer(t)
			ids := sessionIDsFromList(t, ts)
			got, reason := postEventBody(t, ts, ids[0], tc.body)
			if got != tc.want {
				t.Errorf("status = %d, want %d (%s)", got, tc.want, reason)
			}
		})
	}
}

// newSSEDecoder is a minimal SSE parser: enough for `event:` and `data:` lines,
// which is all this server emits.
func newSSEDecoder(body io.Reader) *sseDecoder {
	return &sseDecoder{reader: body}
}

type sseDecoder struct {
	reader io.Reader
	buffer []byte
	chunk  []byte
}

func (d *sseDecoder) next() (sseFrame, error) {
	for {
		if idx := bytes.Index(d.buffer, []byte("\n\n")); idx >= 0 {
			raw := string(d.buffer[:idx])
			d.buffer = d.buffer[idx+2:]
			if frame, ok := parseFrame(raw); ok {
				return frame, nil
			}
			continue
		}
		if d.chunk == nil {
			d.chunk = make([]byte, 4096)
		}
		n, err := d.reader.Read(d.chunk)
		if n > 0 {
			d.buffer = append(d.buffer, d.chunk[:n]...)
		}
		if err != nil {
			return sseFrame{}, err
		}
	}
}

// parseFrame decodes one frame's event name and JSON payload.
func parseFrame(raw string) (sseFrame, bool) {
	frame := sseFrame{}
	var dataLines []string
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if frame.Event == "" || len(dataLines) == 0 {
		return sseFrame{}, false
	}
	if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &frame.Data); err != nil {
		return sseFrame{}, false
	}
	return frame, true
}
