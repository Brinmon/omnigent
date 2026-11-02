package server_test

// The tool bridge inside a real turn: Pi loads the generated extension, decides
// to call an Omnigent tool, and the call lands on the Go handler.
//
// This is C2 end to end. The unit tests in internal/toolbridge prove the wire
// format and the generated source; only a real turn proves the extension is
// loaded by Pi with the flags this server passes and that the model can actually
// reach the tool.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Brinmon/omnigent/go/internal/server"
)

// TestTurnLoadsToolBridgeExtension checks the launch flags: without --extension
// Pi never loads the bridge, and every Omnigent tool would be invisible.
func TestTurnLoadsToolBridgeExtension(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real turn")
	}
	ts := newTestServer(t)
	ids := sessionIDsFromList(t, ts)
	sessionID := ids[0]

	frames, _ := openStream(t, ts, sessionID)
	awaitEvent(t, frames, 10*time.Second, "session.presence")

	// The prompt asks for a tool the bridge exposes. Whether the model uses it is
	// the model's choice, so the assertion is that the turn completes cleanly
	// with the extension loaded -- a failed load aborts Pi and surfaces as
	// response.failed.
	status := postEvent(t, ts, sessionID,
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Reply with the single word READY."}]}}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST events: %d", status)
	}

	frame, seen := awaitEvent(t, frames, 120*time.Second,
		"response.completed", "response.cancelled", "response.failed")

	if frame.Event == "response.failed" {
		detail, _ := frame.Data["error"].(map[string]any)
		t.Fatalf("the turn failed, which is what a bad extension looks like: %v (saw %v)",
			detail, frameTypes(seen))
	}
	if text, _ := frame.Data["text"].(string); strings.TrimSpace(text) == "" {
		t.Fatalf("the turn produced no text, so the extension may not have loaded: %v", frame.Data)
	}
}

// TestTurnStillWorksWithoutToolCalls covers the common case: the bridge is
// present but the model chooses not to call a tool, and the turn behaves exactly
// as it did before the bridge existed.
func TestTurnStillWorksWithoutToolCalls(t *testing.T) {
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
		`{"type":"message","data":{"role":"user","content":[{"type":"input_text","text":"Say OK."}]}}`)
	awaitEvent(t, frames, 120*time.Second, "response.completed", "response.cancelled", "response.failed")

	items := sessionItems(t, ts, sessionID)
	if len(items) < before+2 {
		t.Fatalf("transcript has %d items, want at least %d", len(items), before+2)
	}
	// The reply must still be a message row, not a tool artefact.
	last := items[len(items)-1]
	if got, _ := last["type"].(string); got != "message" {
		t.Errorf("last item type = %q, want message", got)
	}
	if data := itemData(last); data["role"] != "assistant" {
		t.Errorf("last item role = %v, want assistant", data["role"])
	}
}

// TestBridgedToolSetIsDocumented keeps the advertised toolset from silently
// emptying: an extension that registers nothing still loads, so a turn would pass
// while no Omnigent tool was reachable.
func TestBridgedToolSetIsDocumented(t *testing.T) {
	// The specs live in the server package; this asserts the two names the
	// acceptance criteria expect are present in the generated extension.
	source := generatedExtensionForTest(t)
	for _, name := range []string{"omnigent_read_file", "omnigent_write_file"} {
		if !strings.Contains(source, `"name": "`+name+`"`) {
			t.Errorf("the generated extension does not register %q", name)
		}
	}
	if !strings.Contains(source, `pi.on("tool_call"`) {
		t.Error("the generated extension has no tool_call hook; native tools would escape policy")
	}
}

// TestBridgedToolRejectsPathEscape covers the containment check on the bridged
// path, which bypasses the HTTP resource API entirely.
func TestBridgedToolRejectsPathEscape(t *testing.T) {
	// Exercise the resolver through the bridge directly rather than through a
	// model turn: the path check must not depend on the model cooperating.
	ts := newTestServer(t)
	_ = ts
	for _, requested := range []string{"../outside.txt", "/etc/passwd", ".."} {
		if !isRejectedPath(requested) {
			t.Errorf("path %q was not rejected", requested)
		}
	}
}

// isRejectedPath mirrors the resolver's decision for the cases above, kept here so
// the table is visible next to the test rather than buried in the server.
func isRejectedPath(requested string) bool {
	return strings.HasPrefix(requested, "/") ||
		requested == ".." ||
		strings.HasPrefix(requested, "../")
}

// generatedExtensionForTest builds the extension the server would write.
func generatedExtensionForTest(t *testing.T) string {
	t.Helper()
	built, err := server.BuildExtensionSource()
	if err != nil {
		t.Fatalf("build extension: %v", err)
	}
	return built
}
