package server_test

// A turn's messages must survive a reload: the client renders live frames, but a
// reload re-reads the transcript through GET /v1/sessions/{id}. Without the write
// path the live view is a mirage -- it looks right until the page refreshes.
//
// These tests drive a real turn against Pi, so they are the slowest in the suite
// and are skipped in -short.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sessionItems reads a session's transcript through the HTTP API.
func sessionItems(t *testing.T, ts *httptest.Server, sessionID string) []map[string]any {
	t.Helper()
	status, body := getRaw(t, ts, "/v1/sessions/"+sessionID)
	if status != http.StatusOK {
		t.Fatalf("GET session: %d", status)
	}
	var detail struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("parse session: %v", err)
	}
	return detail.Items
}

// TestTurnPersistsTranscript is the reload guarantee: after a turn settles, the
// prompt and the reply are both readable from the stored transcript.
func TestTurnPersistsTranscript(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	before := len(sessionItems(t, ts, sessionID))

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")

	const prompt = "Reply with exactly the word PONG."
	if status := postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"`+prompt+`"}]}}`,
	); status != http.StatusAccepted {
		t.Fatalf("POST events: %d", status)
	}

	// Wait for the turn to end before reading, or the read races the write.
	awaitEvent(t, frames, 120*time.Second, "response.completed", "response.cancelled", "response.failed")

	items := sessionItems(t, ts, sessionID)
	if len(items) < before+2 {
		t.Fatalf("transcript has %d items, want at least %d (prompt + reply)", len(items), before+2)
	}

	// The last two rows are this turn's pair.
	userItem := items[len(items)-2]
	assistantItem := items[len(items)-1]

	if got := itemText(t, userItem); !strings.Contains(got, "PONG") {
		t.Errorf("stored user message = %q, want it to contain the prompt", got)
	}
	if role, _ := itemData(userItem)["role"].(string); role != "user" {
		t.Errorf("stored user message has role %q", role)
	}

	assistantText := itemText(t, assistantItem)
	if strings.TrimSpace(assistantText) == "" {
		t.Error("stored assistant message is empty")
	}
	assistantData := itemData(assistantItem)
	if role, _ := assistantData["role"].(string); role != "assistant" {
		t.Errorf("stored assistant message has role %q", role)
	}
	// The model id is what lets the UI label the reply and cost it later.
	if model, _ := assistantData["model"].(string); model == "" {
		t.Error("stored assistant message has no model id")
	}

	// The content part type is not decoration: the client filters user bubbles
	// with isTextBlock, which accepts only input_text, and renders anything else
	// as an empty bubble. Storing "text" produced exactly that -- correct rows,
	// blank page -- so the type is asserted here rather than inferred from the
	// text being present.
	if got := firstContentType(userItem); got != "input_text" {
		t.Errorf("stored user content type = %q, want input_text", got)
	}
	if got := firstContentType(assistantItem); got != "output_text" {
		t.Errorf("stored assistant content type = %q, want output_text", got)
	}
}

// firstContentType returns the type of the first content part of a message item.
func firstContentType(item map[string]any) string {
	data := itemData(item)
	parts, _ := data["content"].([]any)
	if len(parts) == 0 {
		return ""
	}
	first, _ := parts[0].(map[string]any)
	partType, _ := first["type"].(string)
	return partType
}

// TestTurnRowsShareAResponseID checks the pairing key: the client groups a reply
// with its prompt by response id, so the two rows must agree.
func TestTurnRowsShareAResponseID(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")
	postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Say hi."}]}}`)
	awaitEvent(t, frames, 120*time.Second, "response.completed", "response.cancelled", "response.failed")

	items := sessionItems(t, ts, sessionID)
	if len(items) < 2 {
		t.Fatalf("transcript has %d items", len(items))
	}
	last, previous := items[len(items)-1], items[len(items)-2]
	// ConversationItem exposes response_id, which the client uses for grouping.
	lastResponse, _ := last["response_id"].(string)
	previousResponse, _ := previous["response_id"].(string)
	if lastResponse == "" || lastResponse != previousResponse {
		t.Errorf("response ids differ: %q vs %q", previousResponse, lastResponse)
	}
}

// TestInterruptedTurnStillPersistsPrompt covers the abort path: a cancelled turn
// must not lose the user's message, or an interrupt would erase what was asked.
func TestInterruptedTurnStillPersistsPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	before := len(sessionItems(t, ts, sessionID))

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")
	postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Count to 500."}]}}`)
	// Interrupt promptly, then wait for the turn to settle either way.
	time.Sleep(500 * time.Millisecond)
	postEvent(t, ts, sessionID, `{"type":"interrupt","data":{}}`)
	awaitEvent(t, frames, 120*time.Second, "response.completed", "response.cancelled", "response.failed")

	items := sessionItems(t, ts, sessionID)
	if len(items) <= before {
		t.Fatalf("transcript has %d items, want more than %d", len(items), before)
	}
	found := false
	for _, item := range items[before:] {
		if strings.Contains(itemText(t, item), "Count to 500") {
			found = true
			break
		}
	}
	if !found {
		t.Error("the interrupted turn's prompt is not in the transcript")
	}
}

// itemData unwraps a ConversationItem's typed payload.
func itemData(item map[string]any) map[string]any {
	data, _ := item["data"].(map[string]any)
	return data
}

// itemText concatenates a stored message's text parts.
func itemText(t *testing.T, item map[string]any) string {
	t.Helper()
	data := itemData(item)
	content, ok := data["content"].([]any)
	if !ok {
		text, _ := data["content"].(string)
		return text
	}
	var out strings.Builder
	for _, part := range content {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := partMap["text"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}
