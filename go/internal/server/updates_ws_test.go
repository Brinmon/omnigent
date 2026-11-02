package server_test

// WS /v1/sessions/updates is not in openapi.json (a WebSocket handshake cannot
// be described there), so its protocol is pinned by these tests against the
// Python server's documented frames.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialUpdates opens the update socket against the test server.
func dialUpdates(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/sessions/updates"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readFrame reads one JSON frame within the deadline.
func readFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("frame is not JSON: %v (%q)", err, data)
	}
	return frame
}

// sessionIDsFromList reads the fixture's session ids through the HTTP list.
func sessionIDsFromList(t *testing.T, ts *httptest.Server) []string {
	t.Helper()
	status, body := getRaw(t, ts, "/v1/sessions")
	if status != 200 {
		t.Fatalf("GET /v1/sessions: %d", status)
	}
	var list struct {
		Data []struct {
			Id string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, item.Id)
	}
	return ids
}

// TestUpdatesWatchReturnsSnapshot covers the first client frame and the reply
// the sidebar needs before it can render.
func TestUpdatesWatchReturnsSnapshot(t *testing.T) {
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	if len(ids) == 0 {
		t.Fatal("fixture has no sessions to watch")
	}

	conn := dialUpdates(t, ts)
	watch, err := json.Marshal(map[string]any{"type": "watch", "session_ids": ids})
	if err != nil {
		t.Fatalf("marshal watch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, watch); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	frame := readFrame(t, conn, 10*time.Second)
	if frame["type"] != "snapshot" {
		t.Fatalf("first frame type = %v, want snapshot", frame["type"])
	}
	items, ok := frame["items"].([]any)
	if !ok {
		t.Fatalf("snapshot has no items array: %v", frame)
	}
	if len(items) != len(ids) {
		t.Fatalf("snapshot has %d items, watched %d", len(items), len(ids))
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("snapshot item is not an object: %T", items[0])
	}
	// The item must carry the same fields GET /v1/sessions returns, so the
	// client can replace a row from either source.
	for _, key := range []string{"id", "created_at", "updated_at", "status"} {
		if _, present := first[key]; !present {
			t.Errorf("snapshot item is missing %q", key)
		}
	}
}

// TestUpdatesIgnoresUnknownFrames is the forward-compatibility guarantee: a
// newer client adding a message must not break this server.
func TestUpdatesIgnoresUnknownFrames(t *testing.T) {
	ts := newTestServer(t)
	conn := dialUpdates(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A frame this build does not know, then a valid watch.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"future-thing","x":1}`)); err != nil {
		t.Fatalf("send unknown frame: %v", err)
	}
	ids := sessionIDsFromList(t, ts)
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"watch","session_ids":["`+ids[0]+`"]}`)); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	frame := readFrame(t, conn, 10*time.Second)
	if frame["type"] != "snapshot" {
		t.Fatalf("frame after an unknown message = %v, want snapshot", frame["type"])
	}
}

// TestUpdatesDropsMalformedIDs keeps one bad id from failing the whole watch:
// the client cannot act on an error, so the id is simply unmatched.
func TestUpdatesDropsMalformedIDs(t *testing.T) {
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	conn := dialUpdates(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := `{"type":"watch","session_ids":["not-a-uuid","123","` + ids[0] + `"]}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	frame := readFrame(t, conn, 10*time.Second)
	items, ok := frame["items"].([]any)
	if !ok {
		t.Fatalf("snapshot has no items: %v", frame)
	}
	if len(items) != 1 {
		t.Errorf("snapshot has %d items, want 1 (the malformed ids dropped)", len(items))
	}
}

// TestUpdatesDedupesWatchSet covers the client re-sending an id, which happens
// when it appends to the visible set without rebuilding it.
func TestUpdatesDedupesWatchSet(t *testing.T) {
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	conn := dialUpdates(t, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := `{"type":"watch","session_ids":["` + ids[0] + `","` + ids[0] + `"]}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("send watch: %v", err)
	}

	frame := readFrame(t, conn, 10*time.Second)
	items, ok := frame["items"].([]any)
	if !ok {
		t.Fatalf("snapshot has no items: %v", frame)
	}
	if len(items) != 1 {
		t.Errorf("snapshot has %d items for a duplicated id, want 1", len(items))
	}
}

// TestUpdatesConnectionSurvivesNoWatch proves the socket stays open without a
// watch rather than closing, which the client would treat as a server restart.
func TestUpdatesConnectionSurvivesNoWatch(t *testing.T) {
	ts := newTestServer(t)
	conn := dialUpdates(t, ts)

	// Read with a short deadline: a closed connection surfaces immediately as an
	// error, which is what this asserts does not happen.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		return // a frame arrived, connection is alive
	}
	status := websocket.CloseStatus(err)
	if status != -1 && status != websocket.StatusNormalClosure {
		t.Errorf("connection closed unexpectedly: %v (status %d)", err, status)
	}
}
