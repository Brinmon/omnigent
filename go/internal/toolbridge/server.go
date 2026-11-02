// Package toolbridge serves the loopback tool endpoint Pi's generated extension
// calls.
//
// Pi runs its own tools in-process, so without a bridge Omnigent cannot see or
// gate them. The bridge flips that: the generated extension registers Omnigent's
// tools with Pi, and every call comes back here over a loopback TCP socket,
// authenticated by a per-turn bearer token.
//
// The wire format is one JSON object per line:
//
//	request   {"id":"...","token":"...","tool":"name","args":{...},"call_id":"..."}
//	          {"id":"...","token":"...","kind":"policy_eval","tool":"name","args":{...}}
//	response  {"id":"...","result":...} | {"id":"...","error":"..."}
//	          {"id":"...","verdict":{"block":bool,"reason":"..."}}
//
// Two shapes of request matter:
//
//   - a tool call, which this bridge executes and whose result returns to Pi;
//   - a policy evaluation, which is *only* a verdict. Pi runs the tool itself on
//     allow, so evaluating it here must never execute anything.
package toolbridge

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Request is one frame from the extension.
type Request struct {
	ID     string          `json:"id"`
	Token  string          `json:"token"`
	Kind   string          `json:"kind"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	CallID string          `json:"call_id"`
}

// Response is one frame back to the extension.
type Response struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// Verdict is present only for a policy evaluation.
	Verdict *Verdict `json:"verdict,omitempty"`
}

// Verdict is a TOOL_CALL policy decision for a native (non-bridged) tool.
type Verdict struct {
	Block  bool   `json:"block"`
	Reason string `json:"reason,omitempty"`
}

// ToolHandler executes one bridged tool call.
//
// Returning an error sends the extension an error frame, which it surfaces to Pi
// as a failed tool result rather than a hung call.
type ToolHandler func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error)

// PolicyHandler evaluates a native tool call without executing it.
//
// It returns nil when no policy applies, which the extension treats as "allow"
// and Pi then runs the tool itself.
type PolicyHandler func(ctx context.Context, tool string, args json.RawMessage) (*Verdict, error)

// Server is one turn's tool endpoint.
type Server struct {
	listener net.Listener
	token    string
	execute  ToolHandler
	policy   PolicyHandler

	mu      sync.Mutex
	closed  bool
	clients sync.WaitGroup
}

// NewServer binds a loopback listener and returns a server with a fresh token.
//
// The listener is bound to 127.0.0.1 rather than all interfaces: the bridge is
// reachable only by a process on this host, and the token narrows that further to
// the one Pi child this turn spawned.
func NewServer(execute ToolHandler, policy PolicyHandler) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("tool bridge: listen: %w", err)
	}
	token, err := newToken()
	if err != nil {
		listener.Close()
		return nil, err
	}
	server := &Server{
		listener: listener,
		token:    token,
		execute:  execute,
		policy:   policy,
	}
	go server.serve()
	return server, nil
}

// Port is the bound port, which the generated extension embeds.
func (s *Server) Port() int {
	if address, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return address.Port
	}
	return 0
}

// Token is the bearer token the generated extension embeds.
func (s *Server) Token() string { return s.token }

// Close stops the listener and waits for in-flight clients.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	err := s.listener.Close()
	s.clients.Wait()
	return err
}

// serve accepts connections until the listener closes.
func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		s.clients.Add(1)
		go func() {
			defer s.clients.Done()
			s.handle(conn)
		}()
	}
}

// handle serves one connection's frames.
//
// The extension opens a connection per call and closes it after the response, so
// a connection carries one frame in practice; the loop keeps the protocol honest
// without assuming it.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	// A stalled client must not pin this goroutine for the turn's lifetime.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	reader := bufio.NewReader(conn)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		response := s.dispatch(line)
		encoded, err := json.Marshal(response)
		if err != nil {
			// A tool result may carry a value that cannot be marshalled. Emitting
			// an error frame keeps the extension's promise settling; writing
			// nothing would hang Pi's agent loop.
			encoded, _ = json.Marshal(Response{
				ID:    response.ID,
				Error: "result could not be serialized",
			})
		}
		if _, err := conn.Write(append(encoded, '\n')); err != nil {
			return
		}
	}
}

// dispatch authenticates and routes one frame.
func (s *Server) dispatch(line []byte) Response {
	var request Request
	if err := json.Unmarshal(line, &request); err != nil {
		return Response{Error: "malformed frame"}
	}
	// Constant-time comparison: the token is a secret, and a timing side channel
	// on a loopback socket is still reachable by any local process.
	if !s.tokenOK(request.Token) {
		// The id is echoed so the extension can settle its promise rather than
		// waiting for a timeout; the reason stays vague so a forged frame learns
		// nothing about why it failed.
		return Response{ID: request.ID, Error: "unauthorized"}
	}
	if request.ID == "" || request.Tool == "" {
		return Response{ID: request.ID, Error: "malformed frame"}
	}

	ctx := context.Background()
	if request.Kind == "policy_eval" {
		if s.policy == nil {
			// No policy configured means allow: the extension then runs the tool
			// itself, which is the pre-bridge behaviour.
			return Response{ID: request.ID}
		}
		verdict, err := s.policy(ctx, request.Tool, request.Args)
		if err != nil {
			// Fail open, matching the extension's own transport-error behaviour:
			// a wedged policy must not break Pi's native tools.
			slog.Warn("tool bridge: policy evaluation failed", "tool", request.Tool, "error", err)
			return Response{ID: request.ID}
		}
		return Response{ID: request.ID, Verdict: verdict}
	}

	if s.execute == nil {
		return Response{ID: request.ID, Error: "no tool handler configured"}
	}
	result, err := s.execute(ctx, request.Tool, request.Args, request.CallID)
	if err != nil {
		return Response{ID: request.ID, Error: err.Error()}
	}
	return Response{ID: request.ID, Result: result}
}

// tokenOK compares the presented token in constant time.
func (s *Server) tokenOK(presented string) bool {
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// newToken returns a url-safe 256-bit token, matching the Python side's
// “secrets.token_urlsafe(32)“.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("tool bridge: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ErrClosed reports a request to a closed bridge.
var ErrClosed = errors.New("tool bridge is closed")

// hmacEqual is exported for tests that need to compare tokens the way the bridge
// does.
func hmacEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}
