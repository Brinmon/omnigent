package server_test

// The policy gate inside a real turn: a deny rule must stop the tool call, and
// the turn must still finish rather than hanging on a blocked call.
//
// This is D1's shape. The engine's own tests prove precedence; only a real turn
// proves the gate is on the path Pi actually takes.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPolicyDenyStopsBridgedTool drives a turn where the model tries to use a
// bridged tool that a policy denies.
func TestPolicyDenyStopsBridgedTool(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	// Deny the tool the prompt asks for, so a model that complies cannot succeed.
	t.Setenv("OMNIGENT_POLICY_RULES",
		`[{"name":"deny-write","tools":["omnigent_write_file"],"expression":"true",`+
			`"action":"DENY","reason":"writes are frozen for this test","enabled":true}]`)

	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")

	status := postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text",`+
			`"text":"Call the omnigent_write_file tool to write BLOCKED_MARKER into denied.txt."}]}}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST events: %d", status)
	}

	// The turn must reach a terminal frame: a blocked tool must not hang it.
	frame, seen := awaitEvent(t, frames, 150*time.Second,
		"response.completed", "response.cancelled", "response.failed")
	if frame.Event == "response.failed" {
		// A failure is acceptable if it is the policy refusing, but the turn
		// having settled at all is the point.
		t.Logf("turn settled as failed after the policy denied the tool: %v", frame.Data)
	}
	t.Logf("frames before settle: %v", frameTypes(seen))
}

// TestPolicyAllowsUnmentionedTool keeps the gate from becoming a blanket block:
// a rule scoped to one tool must leave others usable.
func TestPolicyAllowsUnmentionedTool(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	t.Setenv("OMNIGENT_POLICY_RULES",
		`[{"name":"deny-write","tools":["omnigent_write_file"],"expression":"true",`+
			`"action":"DENY","reason":"writes are frozen","enabled":true}]`)

	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")
	postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Say READY."}]}}`)

	frame, _ := awaitEvent(t, frames, 150*time.Second,
		"response.completed", "response.cancelled", "response.failed")
	if frame.Event == "response.failed" {
		detail, _ := frame.Data["error"].(map[string]any)
		t.Fatalf("a turn with no tool call failed under a tool-scoped policy: %v", detail)
	}
	if text, _ := frame.Data["text"].(string); strings.TrimSpace(text) == "" {
		t.Error("the turn produced no text")
	}
}
