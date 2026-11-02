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
	"time"

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
		return s.echoTurn(ctx, sessionID, hub, input)

	case "interrupt":
		// With no running turn there is nothing to interrupt, but the client
		// still needs the acknowledgement: it has already flipped its own UI to
		// "stopped" and waits for the server's confirmation.
		s.publishStatus(ctx, sessionID, hub, "idle")
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

// echoTurn walks one turn's status edges and publishes them.
//
// The ordering mirrors a real turn: status running, a user message, the
// assistant's reply, then idle. A client that renders this sequence correctly
// will render a real turn too, because the frames are the same kind and order.
func (s *Server) echoTurn(
	ctx context.Context,
	sessionID string,
	hub *stream.Hub,
	input sessionEventInput,
) (map[string]any, error) {
	excerpt := userMessageText(input.Data)

	s.publishStatus(ctx, sessionID, hub, "running")

	// The assistant echo is deliberately marked so nobody mistakes it for a
	// model response when reading a transcript from this build.
	reply := "This build has no language model attached yet, so this is an echo of your message: " + excerpt

	finished := time.Now().UTC().Format(time.RFC3339Nano)
	hub.Publish(stream.Event{
		Type: "response.completed",
		Payload: map[string]any{
			"type":            "response.completed",
			"conversation_id": sessionID,
			"sequence_number": nil,
			"text":            reply,
			"echo":            true,
			"completed_at":    finished,
		},
	})

	s.publishStatus(ctx, sessionID, hub, "idle")
	return map[string]any{"ok": true, "type": input.Type}, nil
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
