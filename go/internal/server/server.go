// Package server wires the generated API interface to real data.
//
// P1 scope is the read surface the web client needs to boot and render the
// sidebar: GET /v1/info, /v1/me, /v1/harnesses, and the /v1/sessions read
// paths. Everything else returns a coded 501 so a missing implementation shows
// up as a clear error rather than a silently empty 200.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Brinmon/omnigent/go/internal/api"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
	"github.com/Brinmon/omnigent/go/internal/stream"
)

// Server implements api.ServerInterface.
//
// The interface is generated from openapi.json, so this type cannot drift from
// the contract: a missing or renamed operation stops the build.
type Server struct {
	queries   *store.Queries
	version   string
	workspace int64
	// userID is the caller identity used for the session grant check. P1 is
	// single-user local, so it is fixed; a multi-user build derives it from the
	// request.
	userID string
	// streams owns the live event fan-out, one hub per session.
	streams *stream.Registry
	// streamEpoch identifies this process's stream generation so a client that
	// reconnects after a restart knows its cached snapshot may be stale.
	streamEpoch string
	// pi is how to launch the harness, or why it cannot be launched. Resolved
	// once at startup so a turn does not pay for the lookup, and so a build with
	// no Pi installed still serves the read API and can say why.
	pi piConfig
	// activeTurns tracks the running turn per session so an interrupt can cancel
	// exactly one, and so a second message during a turn is refused rather than
	// queued behind an unbounded number of children.
	turnsMu     sync.Mutex
	activeTurns map[string]context.CancelFunc
}

// compile-time proof that this type satisfies the generated interface. The
// interface declares all 154 operations; stubs.go supplies the rest.
var _ api.ServerInterface = (*Server)(nil)

// New builds a Server over the given database.
func New(conn *sql.DB, version string) *Server {
	return &Server{
		queries:     store.New(conn),
		version:     version,
		userID:      "local",
		streams:     stream.NewRegistry(),
		streamEpoch: streamEpochValue(os.Getpid(), time.Now()),
		pi:          resolvePiConfig(),
		activeTurns: make(map[string]context.CancelFunc),
	}
}

// Router returns the HTTP handler for the full API surface.
//
// The generated wrapper routes every documented operation to this type, so the
// router shape comes from openapi.json rather than a hand-maintained list.
func (s *Server) Router() http.Handler {
	router := chi.NewRouter()
	router.Use(recoverJSON)
	// The WebSocket handshake cannot be described by openapi.json, so its
	// routes are mounted beside the generated ones rather than through
	// HandlerFromMux (which would answer a handshake with 426 Upgrade
	// Required).
	// Register the WebSocket route before the generated ones so it wins the
	// match; HandlerFromMux would otherwise answer the upgrade with 426.
	router.Get("/v1/sessions/updates", s.handleSessionUpdates)
	router.Post("/v1/sessions/{session_id}/events", s.PostEventV1SessionsSessionIdEventsPost)
	api.HandlerFromMux(s, router)
	return router
}

// recoverJSON turns a panic into a JSON 500.
//
// Without this a panic reaches net/http's default handler and becomes a plain
// text body the web client cannot parse -- the same failure the Python runner
// had before its catch-all handler was added.
func recoverJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeError(w, http.StatusInternalServerError, "internal_error",
					"unexpected error while handling the request")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// HealthHealthGet implements GET /health.
//
// The server is its own health signal: by the time a request reaches this
// handler the database pool is open and the router is serving. Answering 200
// also keeps readiness checks working.
func (s *Server) HealthHealthGet(w http.ResponseWriter, r *http.Request, params api.HealthHealthGetParams) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// MeV1MeGet implements GET /v1/me.
//
// P1 is single-user local, so the identity is fixed rather than derived from a
// session -- matching what the Python server reports in that mode.
func (s *Server) MeV1MeGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  "local",
		"is_admin": true,
	})
}

// ListSessionsV1SessionsGet implements GET /v1/sessions.
func (s *Server) ListSessionsV1SessionsGet(
	w http.ResponseWriter,
	r *http.Request,
	params api.ListSessionsV1SessionsGetParams,
) {
	// The document defaults to 20, matching the Python route; a client that
	// wants more passes ?limit=.
	limit := int64(20)
	if params.Limit != nil {
		limit = int64(*params.Limit)
	}
	includeArchived := false
	if params.IncludeArchived != nil {
		includeArchived = *params.IncludeArchived
	}
	rows, err := s.queries.ListSessions(r.Context(), store.ListSessionsParams{
		WorkspaceID:     s.workspace,
		UserID:          s.userID,
		IncludeArchived: includeArchived,
		PageLimit:       limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not list sessions")
		return
	}

	items := make([]api.SessionListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, s.sessionListItem(r, row))
	}

	// Pagination cursors are explicit nulls on a single short page: the client
	// reads them to decide whether to ask for more, so an absent key would be
	// ambiguous.
	object := api.SessionListObject("list")
	hasMore := false
	var firstID, lastID *string
	if len(items) > 0 {
		first := items[0].Id
		last := items[len(items)-1].Id
		firstID, lastID = &first, &last
	}
	writeJSON(w, http.StatusOK, api.SessionList{
		Object:  &object,
		Data:    &items,
		FirstId: firstID,
		LastId:  lastID,
		HasMore: &hasMore,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────

// sessionListItem renders one stored row as the API's SessionListItem.
//
// Ids go out as bare 32-char hex and timestamps as Unix epoch seconds, which is
// what the document declares and what the client's date formatting expects.
func (s *Server) sessionListItem(r *http.Request, row store.Conversation) api.SessionListItem {
	return s.sessionListItemFor(r.Context(), row)
}

// sessionListItemFor is sessionListItem without an *http.Request, for callers
// that only hold a context (the WebSocket diff loop).
func (s *Server) sessionListItemFor(ctx context.Context, row store.Conversation) api.SessionListItem {
	// agent_id is nullable on disk but a required string in the document, so a
	// session with no bound agent reports the empty string rather than a
	// fabricated id.
	agentID := ""
	if !row.AgentID.IsZero() {
		agentID = row.AgentID.String()
	}
	// These fields are pointers in the generated type because the document
	// marks them optional, but the Python server always emits them. Leaving a
	// pointer nil would drop the key from JSON (omitempty), so the client's
	// `item.labels ?? {}` reads an absent field instead of an empty one. Filling
	// every pointer keeps the response shape identical.
	zero := int64(0)
	archived := row.Archived
	pendingElicitations := zero
	owner := s.userID
	permissionLevel := int64(levelOwner)
	unread := false
	labels := map[string]string{}
	title := row.Title
	status := api.SessionListItemStatusIdle

	// Comments drive a change fingerprint on the client: the count catches a
	// delete, the timestamp catches an add or an edit. A session with no comments
	// reports a zero count and no timestamp, matching the Python server (which
	// omits the key rather than sending 0).
	commentsCount, commentsUpdatedAt := s.commentFingerprint(ctx, row.ID)

	item := api.SessionListItem{
		Id:                       row.ID.String(),
		AgentId:                  agentID,
		Archived:                 &archived,
		CommentsCount:            &commentsCount,
		CommentsUpdatedAt:        commentsUpdatedAt,
		CreatedAt:                row.CreatedAt,
		UpdatedAt:                row.UpdatedAt,
		Labels:                   &labels,
		Owner:                    &owner,
		PendingElicitationsCount: &pendingElicitations,
		PermissionLevel:          &permissionLevel,
		Status:                   status,
		Title:                    &title,
		ViewerUnread:             &unread,
	}
	if !row.ParentConversationID.IsZero() {
		parentID := row.ParentConversationID.String()
		item.ParentSessionId = &parentID
	}
	// The sidebar shows the bound agent's name, so resolve it once per row.
	// A dangling agent_id leaves the field absent, matching the Python server's
	// "None when the agent row cannot be found".
	if !row.AgentID.IsZero() {
		if agent, err := s.queries.GetAgent(ctx, store.GetAgentParams{
			WorkspaceID: s.workspace,
			ID:          row.AgentID,
		}); err == nil {
			name := agent.Name
			item.AgentName = &name
		}
	}
	return item
}

// commentFingerprint returns this session's comment count and the newest
// mutation timestamp, or a zero count and nil timestamp when it has none.
//
// A query failure is not fatal for the list: the fingerprint is a cache
// invalidation hint, so a session still renders without it.
func (s *Server) commentFingerprint(ctx context.Context, id db.Uuid16) (int64, *int64) {
	row, err := s.queries.GetCommentFingerprint(ctx, store.GetCommentFingerprintParams{
		WorkspaceID:    s.workspace,
		ConversationID: id[:],
	})
	if err != nil {
		return 0, nil
	}
	var last *int64
	switch value := row.LastUpdatedAt.(type) {
	case int64:
		last = &value
	case int32:
		converted := int64(value)
		last = &converted
	case float64:
		converted := int64(value)
		last = &converted
	}
	return row.CommentCount, last
}

// levelOwner is the permission level the Python server records for a session's
// creator (omnigent.server.auth.LEVEL_OWNER). P1 is single-user local, so the
// caller owns every session.
const levelOwner int64 = 4

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so an encode failure cannot be corrected
	// here; the client sees a truncated body and reports its own parse error.
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the error envelope the document declares.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// notImplemented answers a documented operation P1 does not implement yet.
//
// A coded 501 is deliberate: the route exists, so the contract holds, but a
// client asking for it learns it is unimplemented instead of receiving an empty
// 200 it would render as "no data".
func notImplemented(w http.ResponseWriter, operation string) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		operation+" is not implemented in this build yet")
}
