package server_test

// Tool calls must reach the transcript, not only the live stream. A reload that
// shows the assistant's reply but not the tool call it made leaves the reader
// unable to tell why the reply says what it says.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestToolCallIsPersisted drives a turn where the model uses the bridged tool and
// checks the transcript afterwards.
func TestToolCallIsPersisted(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	before := len(sessionItems(t, ts, sessionID))

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")

	status := postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text",`+
			`"text":"Call the omnigent_write_file tool to write the text UNIT_MARKER into probe_out.txt, then reply with the single word DONE."}]}}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST events: %d", status)
	}

	frame, seen := awaitEvent(t, frames, 150*time.Second,
		"response.completed", "response.cancelled", "response.failed")
	if frame.Event == "response.failed" {
		detail, _ := frame.Data["error"].(map[string]any)
		t.Fatalf("turn failed: %v (saw %v)", detail, frameTypes(seen))
	}

	items := sessionItems(t, ts, sessionID)
	if len(items) <= before {
		t.Fatalf("transcript has %d items, want more than %d", len(items), before)
	}

	var (
		sawCall   bool
		sawOutput bool
		callName  string
	)
	for _, item := range items[before:] {
		itemType, _ := item["type"].(string)
		data := itemData(item)
		switch itemType {
		case "function_call":
			sawCall = true
			callName, _ = data["name"].(string)
			// The arguments column holds a JSON string, which is how the Python
			// store writes it and what its reader expects.
			arguments, ok := data["arguments"].(string)
			if !ok {
				t.Errorf("function_call arguments are %T, want a JSON string", data["arguments"])
			} else if !strings.HasPrefix(strings.TrimSpace(arguments), "{") {
				t.Errorf("function_call arguments are not JSON: %q", arguments)
			}
			if callID, _ := data["call_id"].(string); callID == "" {
				t.Error("function_call has no call_id; the output cannot be paired with it")
			}
		case "function_call_output":
			sawOutput = true
			if callID, _ := data["call_id"].(string); callID == "" {
				t.Error("function_call_output has no call_id")
			}
		}
	}

	if !sawCall {
		t.Fatalf("no function_call row; the model may not have used the tool. Types seen: %v",
			itemTypes(items[before:]))
	}
	if callName == "" || !strings.HasPrefix(callName, "omnigent_") {
		t.Errorf("persisted tool name = %q, want an omnigent_* tool", callName)
	}
	if !sawOutput {
		t.Error("no function_call_output row; the call would render without its result")
	}
}

// itemTypes lists the item types in a slice, for a diagnostic message.
func itemTypes(items []map[string]any) []string {
	types := make([]string, 0, len(items))
	for _, item := range items {
		itemType, _ := item["type"].(string)
		types = append(types, itemType)
	}
	return types
}
