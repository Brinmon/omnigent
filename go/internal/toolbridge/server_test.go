package toolbridge_test

// The bridge is a loopback socket with a bearer token, so its tests cover the
// three things that would otherwise be found in production: the wire format, the
// authentication, and the difference between executing a tool and merely
// evaluating a policy for one.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Brinmon/omnigent/go/internal/toolbridge"
)

// sendFrame opens a connection, writes one frame, and returns the decoded reply.
func sendFrame(t *testing.T, port int, frame map[string]any) map[string]any {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("reply is not JSON: %v (%q)", err, line)
	}
	return reply
}

// TestBridgeExecutesTool covers the happy path: a bridged call reaches the
// handler and its result comes back.
func TestBridgeExecutesTool(t *testing.T) {
	var (
		mu       sync.Mutex
		gotTool  string
		gotArgs  string
		gotCall  string
		executed bool
	)
	server, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			gotTool, gotArgs, gotCall, executed = tool, string(args), callID, true
			return map[string]any{"content": "file contents"}, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	reply := sendFrame(t, server.Port(), map[string]any{
		"id":      "req-1",
		"token":   server.Token(),
		"tool":    "sys_os_read",
		"args":    map[string]any{"path": "a.txt"},
		"call_id": "call-9",
	})

	if reply["id"] != "req-1" {
		t.Errorf("reply id = %v, want req-1", reply["id"])
	}
	if _, failed := reply["error"]; failed {
		t.Fatalf("bridge returned an error: %v", reply["error"])
	}
	if !executed {
		t.Fatal("the tool handler was never called")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotTool != "sys_os_read" {
		t.Errorf("tool = %q, want sys_os_read", gotTool)
	}
	if !strings.Contains(gotArgs, "a.txt") {
		t.Errorf("args = %q, want them to carry the path", gotArgs)
	}
	if gotCall != "call-9" {
		t.Errorf("call_id = %q, want call-9", gotCall)
	}
}

// TestBridgeRejectsWrongToken is the authentication check. The reply must carry
// the id so the extension can settle rather than time out, and must not execute
// anything.
func TestBridgeRejectsWrongToken(t *testing.T) {
	var executed bool
	server, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			executed = true
			return "should not happen", nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	for _, token := range []string{"", "wrong", server.Token() + "x"} {
		reply := sendFrame(t, server.Port(), map[string]any{
			"id": "req-2", "token": token, "tool": "sys_os_read", "args": map[string]any{},
		})
		if reply["error"] != "unauthorized" {
			t.Errorf("token %q: error = %v, want unauthorized", token, reply["error"])
		}
		if reply["id"] != "req-2" {
			t.Errorf("token %q: reply id = %v; the extension cannot settle without it", token, reply["id"])
		}
	}
	if executed {
		t.Error("an unauthorized frame executed a tool")
	}
}

// TestPolicyEvalDoesNotExecute is the distinction the design turns on: a policy
// evaluation returns a verdict and must never run the tool, because Pi runs it
// itself on allow.
func TestPolicyEvalDoesNotExecute(t *testing.T) {
	var (
		mu            sync.Mutex
		executed      bool
		policyQueried bool
	)
	server, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			mu.Lock()
			executed = true
			mu.Unlock()
			return "executed", nil
		},
		func(ctx context.Context, tool string, args json.RawMessage) (*toolbridge.Verdict, error) {
			mu.Lock()
			policyQueried = true
			mu.Unlock()
			return &toolbridge.Verdict{Block: true, Reason: "policy says no"}, nil
		},
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	reply := sendFrame(t, server.Port(), map[string]any{
		"id": "req-3", "token": server.Token(), "kind": "policy_eval",
		"tool": "bash", "args": map[string]any{"command": "rm -rf /"},
	})

	mu.Lock()
	defer mu.Unlock()
	if executed {
		t.Fatal("a policy evaluation executed the tool; Pi would then run it too")
	}
	if !policyQueried {
		t.Fatal("the policy handler was never called")
	}
	verdict, ok := reply["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no verdict: %v", reply)
	}
	if verdict["block"] != true {
		t.Errorf("verdict block = %v, want true", verdict["block"])
	}
	if verdict["reason"] != "policy says no" {
		t.Errorf("verdict reason = %v", verdict["reason"])
	}
}

// TestPolicyEvalWithoutHandlerAllows covers a turn with no policy wired: the
// bridge answers with no verdict, which the extension treats as allow and Pi then
// runs the tool. That is the pre-bridge behaviour, and it must not be an error.
func TestPolicyEvalWithoutHandlerAllows(t *testing.T) {
	server, err := toolbridge.NewServer(nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	reply := sendFrame(t, server.Port(), map[string]any{
		"id": "req-4", "token": server.Token(), "kind": "policy_eval",
		"tool": "bash", "args": map[string]any{},
	})
	if _, failed := reply["error"]; failed {
		t.Errorf("no policy handler produced an error: %v", reply["error"])
	}
	if _, present := reply["verdict"]; present {
		t.Error("a nil verdict should be absent, not sent as an empty object")
	}
}

// TestPolicyEvalFailsOpen covers a policy handler that errors: an allow is the
// right answer, because a wedged policy must not break Pi's native tools.
func TestPolicyEvalFailsOpen(t *testing.T) {
	server, err := toolbridge.NewServer(nil,
		func(ctx context.Context, tool string, args json.RawMessage) (*toolbridge.Verdict, error) {
			return nil, fmt.Errorf("policy engine unavailable")
		},
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	reply := sendFrame(t, server.Port(), map[string]any{
		"id": "req-5", "token": server.Token(), "kind": "policy_eval",
		"tool": "bash", "args": map[string]any{},
	})
	if _, failed := reply["error"]; failed {
		t.Errorf("a policy error surfaced as a bridge error: %v", reply["error"])
	}
	if _, present := reply["verdict"]; present {
		t.Error("a failed evaluation produced a verdict; it should allow by omission")
	}
}

// TestToolErrorBecomesErrorFrame keeps a failing tool from hanging the turn: the
// extension needs a frame to settle its promise.
func TestToolErrorBecomesErrorFrame(t *testing.T) {
	server, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			return nil, fmt.Errorf("permission denied")
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	reply := sendFrame(t, server.Port(), map[string]any{
		"id": "req-6", "token": server.Token(), "tool": "sys_os_write", "args": map[string]any{},
	})
	if !strings.Contains(fmt.Sprint(reply["error"]), "permission denied") {
		t.Errorf("error = %v, want the handler's message", reply["error"])
	}
}

// TestMalformedFrameDoesNotCrash covers a bad frame: the connection answers with
// an error and the server keeps serving.
func TestMalformedFrameDoesNotCrash(t *testing.T) {
	server, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			return "ok", nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	// A frame with no id and no tool is malformed even with a valid token.
	reply := sendFrame(t, server.Port(), map[string]any{
		"id": "", "token": server.Token(), "tool": "",
	})
	if reply["error"] == nil {
		t.Error("a malformed frame was accepted")
	}

	// The next well-formed frame still works, so the listener survived.
	good := sendFrame(t, server.Port(), map[string]any{
		"id": "req-7", "token": server.Token(), "tool": "sys_os_read", "args": map[string]any{},
	})
	if good["error"] != nil {
		t.Errorf("the server stopped serving after a malformed frame: %v", good["error"])
	}
}

// TestServerCloseStopsAccepting covers shutdown: a turn's bridge must release its
// port when the turn ends.
func TestServerCloseStopsAccepting(t *testing.T) {
	server, err := toolbridge.NewServer(nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	port := server.Port()
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A second Close must be safe: the turn's cleanup runs on more than one path.
	if err := server.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("the bridge still accepts connections after Close")
	}
}

// TestTokenIsLongAndURLSafe pins the token shape to the Python side's
// “secrets.token_urlsafe(32)“: 32 random bytes, base64url without padding.
func TestTokenIsLongAndURLSafe(t *testing.T) {
	server, err := toolbridge.NewServer(nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	token := server.Token()
	// 32 bytes base64url-encoded is 43 characters.
	if len(token) != 43 {
		t.Errorf("token length = %d, want 43 (32 bytes base64url)", len(token))
	}
	for _, r := range token {
		valid := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !valid {
			t.Errorf("token contains %q, which is not url-safe base64", r)
			break
		}
	}
}

// TestEachServerGetsItsOwnToken keeps turns isolated: one turn's extension must
// not be able to call another turn's bridge.
func TestEachServerGetsItsOwnToken(t *testing.T) {
	first, err := toolbridge.NewServer(nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer first.Close()
	second, err := toolbridge.NewServer(nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer second.Close()

	if first.Token() == second.Token() {
		t.Error("two bridges share a token; one turn's extension could call another's tools")
	}
	if first.Port() == second.Port() {
		t.Error("two bridges share a port")
	}
}
