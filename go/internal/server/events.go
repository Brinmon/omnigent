package server

// POST /v1/sessions/{id}/events accepts client input and drives a turn.
//
// The route is marked include_in_schema=false in the Python server because its
// response is an acknowledgement, not a domain model, so openapi.json does not
// describe it. The generated router therefore has no handler for it and this
// file registers it directly.
//
// P2 has no runner and no harness, so a turn here is an *echo* turn: the input
// is persisted as a user message, the session's status walks the same edges a
// real turn does, an assistant message is appended, and every transition is
// published on the session stream. That is enough for the web client to render
// a real conversation end to end, and it exercises the event sequence B4
// asserts -- while making it explicit that no model was called.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/stream"
)

// maxSessionEventBatch mirrors the Python server's limit.
const maxSessionEventBatch = 100

// sessionEventInput is one client-submitted event.
//
// The shape matches SessionEventInput in the Python schemas: a discriminator
// plus a free-form payload the route layer interprets per type.
type sessionEventInput struct {
	Type          string         `json:"type"`
	Data          map[string]any `json:"data"`
	ModelOverride *string        `json:"model_override,omitempty"`
	CreatedBy     *string        `json:"created_by,omitempty"`
}

// userMessageText pulls the plain text out of a message event's payload.
//
// A message's content is a list of typed parts (input_text, input_image, ...).
// Only text parts are echoed; an image-only message still produces a turn, just
// with an empty excerpt.
func userMessageText(data map[string]any) string {
	content, ok := data["content"]
	if !ok {
		return ""
	}
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var text string
		for _, part := range value {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if fragment, ok := partMap["text"].(string); ok {
				if text != "" {
					text += "\n"
				}
				text += fragment
			}
		}
		return text
	}
	return ""
}

// PostEventV1SessionsSessionIdEventsPost implements POST .../events.
//
// A single object and a top-level array are both accepted; the array form is
// processed in order and is not atomic, matching the Python server.
func (s *Server) PostEventV1SessionsSessionIdEventsPost(w http.ResponseWriter, r *http.Request) {
	sessionID := pathParam(r, "session_id")
	id, err := db.ParseUuid16(sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	row, err := s.queries.GetSession(r.Context(), querySession(s.workspace, id))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	_ = row

	body, err := readEventBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"session event batch must not be empty")
		return
	}
	if len(body) > maxSessionEventBatch {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"session event batch exceeds the 100-event limit")
		return
	}

	acks := make([]map[string]any, 0, len(body))
	for _, input := range body {
		ack, err := s.applyEvent(r.Context(), id.String(), input)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		acks = append(acks, ack)
	}

	// 202: the events are accepted and the turn runs asynchronously. A client
	// follows progress on the session stream, not on this response.
	if len(acks) == 1 {
		writeJSON(w, http.StatusAccepted, acks[0])
		return
	}
	writeJSON(w, http.StatusAccepted, acks)
}

// readEventBody decodes either a single event or an array of them.
func readEventBody(r *http.Request) ([]sessionEventInput, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, errors.New("request body is not valid JSON")
	}
	trimmed := firstNonSpace(raw)
	if trimmed == '[' {
		var batch []sessionEventInput
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, errors.New("request body is not a valid event array")
		}
		return batch, nil
	}
	var single sessionEventInput
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, errors.New("request body is not a valid event object")
	}
	return []sessionEventInput{single}, nil
}

func firstNonSpace(raw []byte) byte {
	for _, b := range raw {
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return b
	}
	return 0
}

// applyEvent persists one input and, for a message, runs an echo turn.
func (s *Server) applyEvent(ctx context.Context, sessionID string, input sessionEventInput) (map[string]any, error) {
	hub := s.streams.ForSession(sessionID)

	switch input.Type {
	case "message":
		if err := s.runTurn(ctx, sessionID, input); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "type": input.Type}, nil

	case "interrupt":
		s.interruptTurn(ctx, sessionID, hub)
		return map[string]any{"ok": true, "type": input.Type}, nil

	case "approval", "function_call_output":
		// A tool resolution belongs to a parked turn. P2 has no tool loop, so
		// the event is acknowledged and dropped rather than persisted as a row
		// that would render as a dangling tool result.
		return map[string]any{"ok": true, "type": input.Type}, nil

	default:
		return nil, errors.New("unsupported event type " + input.Type)
	}
}

// runTurn starts a real turn against Pi.
//
// The turn runs asynchronously: the POST acknowledges acceptance and the client
// follows progress on the session stream, which is the protocol the web app
// already implements. A start failure is reported on this response so the client
// does not wait for events that will never come.
func (s *Server) runTurn(
	ctx context.Context,
	sessionID string,
	input sessionEventInput,
) error {
	excerpt := userMessageText(input.Data)
	if excerpt == "" {
		return errors.New("message has no text content")
	}
	hub := s.streams.ForSession(sessionID)

	s.turnsMu.Lock()
	if _, busy := s.activeTurns[sessionID]; busy {
		s.turnsMu.Unlock()
		// A second message during a turn would spawn a second child; the client
		// is expected to steer or wait, so this is a client error rather than a
		// queue.
		return errors.New("a turn is already running for this session")
	}
	turnCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), piTurnTimeout)
	s.activeTurns[sessionID] = cancel
	s.turnsMu.Unlock()

	// runPiTurn runs the whole turn and returns when it ends, so this goroutine
	// owns the registry slot for the turn's full lifetime. Releasing the slot
	// when runPiTurn merely *launched* the child would kill it immediately.
	go func() {
		defer func() {
			s.turnsMu.Lock()
			delete(s.activeTurns, sessionID)
			s.turnsMu.Unlock()
			cancel()
		}()
		if err := s.runPiTurn(turnCtx, sessionID, hub, excerpt); err != nil {
			s.publishPiFailure(turnCtx, sessionID, hub, "harness", err.Error())
		}
	}()
	return nil
}

// interruptTurn cancels the session's running turn, if any.
//
// With no turn in flight there is nothing to stop, but the client still gets its
// acknowledgement: it has already flipped its own UI and waits for the server's
// confirmation.
func (s *Server) interruptTurn(ctx context.Context, sessionID string, hub *stream.Hub) {
	s.turnsMu.Lock()
	cancel, running := s.activeTurns[sessionID]
	s.turnsMu.Unlock()
	if running {
		cancel()
		return
	}
	s.publishStatus(ctx, sessionID, hub, "idle")
}

// publishStatus emits a status transition.
func (s *Server) publishStatus(ctx context.Context, sessionID string, hub *stream.Hub, status string) {
	hub.Publish(stream.SessionStatus(sessionID, status))
}

// pathParam reads a chi URL parameter.
//
// The generated router supplies path values as function arguments, but this
// route is registered directly, so it reads them from the request context.
func pathParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}
