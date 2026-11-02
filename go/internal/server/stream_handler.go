package server

// The session SSE stream.
//
// GET /v1/sessions/{id}/stream is a snapshot-plus-live-tail stream: it does not
// replay history, so a client reconciles by reading GET /v1/sessions/{id} first
// and then following this stream. Two consequences shape the handler:
//
//   - the first frames are the session's current state (presence, child
//     sessions, changed files), not a log;
//   - the connection must never be buffered by an intermediary, or deltas and
//     heartbeats arrive in bursts after an idle timeout instead of steadily.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Brinmon/omnigent/go/internal/api"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
	"github.com/Brinmon/omnigent/go/internal/stream"
)

// streamEpochHeader identifies one server process's stream generation.
//
// A client that reconnects and sees a different epoch knows the server
// restarted and its cached state may be stale, so it refetches the snapshot
// instead of trusting the tail.
const streamEpochHeader = "X-Omnigent-Stream-Epoch"

// sseHeaders are the response headers the Python server sets, reproduced so a
// proxy behaves identically against either build.
//
// x-accel-buffering disables nginx's response buffering; without it, SSE events
// sit in the proxy until its buffer fills and the UI appears frozen.
var sseHeaders = map[string]string{
	"Content-Type":      "text/event-stream; charset=utf-8",
	"Cache-Control":     "no-cache",
	"X-Accel-Buffering": "no",
}

// StreamSessionV1SessionsSessionIdStreamGet implements the session stream.
func (s *Server) StreamSessionV1SessionsSessionIdStreamGet(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
	params api.StreamSessionV1SessionsSessionIdStreamGetParams,
) {
	id, err := db.ParseUuid16(sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	// The stream only exists for a session that does; answering 200 for an
	// unknown id would leave the client waiting for events that never come.
	if _, err := s.queries.GetSession(r.Context(), querySession(s.workspace, id)); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without a flusher the stream would buffer until the handler returns,
		// which for a live tail is never. Say so plainly rather than hanging.
		writeError(w, http.StatusInternalServerError, "internal_error",
			"streaming is not supported by this server configuration")
		return
	}

	// Subscribe before writing headers so an event published during setup is
	// not lost between "connected" and "listening".
	hub := s.streams.ForSession(sessionID)
	events, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	for key, value := range sseHeaders {
		w.Header().Set(key, value)
	}
	w.Header().Set(streamEpochHeader, s.streamEpoch)
	w.WriteHeader(http.StatusOK)

	// The opening frames describe current state, matching the Python server's
	// snapshot-first ordering.
	if err := writeEvent(w, stream.SessionPresence(sessionID, s.presenceViewers(hub))); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(stream.HeartbeatInterval)
	defer heartbeat.Stop()

	// The request context ends when the client disconnects, which is the only
	// signal a streaming handler gets.
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-events:
			if !open {
				// The hub dropped this subscriber for falling behind. Ending
				// the response makes the client reconnect, which resynchronises
				// from the snapshot rather than silently missing events.
				return
			}
			if err := writeEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if err := writeEvent(w, stream.SessionHeartbeat(sessionID)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent renders and writes one frame.
func writeEvent(w http.ResponseWriter, event stream.Event) error {
	_, err := w.Write(event.Encode())
	return err
}

// presenceViewers reports the co-viewers of a session.
//
// P1/P2 has no viewer registry, so a local single-user install reports an empty
// list -- which is what the Python server reports when the only viewer is the
// caller (it excludes the requester). The field is presence-with-an-empty-list
// rather than absent, because the client diffs it to decide whether to re-render
// the avatar strip.
func (s *Server) presenceViewers(hub *stream.Hub) []map[string]any {
	return []map[string]any{}
}

// streamEpochValue builds the per-process epoch token.
func streamEpochValue(pid int, started time.Time) string {
	return fmt.Sprintf("%d-%x", pid, started.UnixNano())
}

// querySession is the GetSession parameter shape, kept local so the stream
// handler does not import the store package for one struct literal.
func querySession(workspace int64, id db.Uuid16) store.GetSessionParams {
	return store.GetSessionParams{WorkspaceID: workspace, ID: id}
}
