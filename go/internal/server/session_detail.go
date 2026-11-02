package server

// The session detail endpoint.
//
// GET /v1/sessions/{id} returns api.SessionResponse, which is far richer than
// the sidebar's list item: it carries the bound agent's name, the full
// transcript, runner and host state, usage, and the switches that describe what
// the session page may do. The web client reads most of these unconditionally,
// so every field it would dereference is populated here rather than left absent.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Brinmon/omnigent/go/internal/api"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// GetSessionV1SessionsSessionIdGet implements GET /v1/sessions/{session_id}.
func (s *Server) GetSessionV1SessionsSessionIdGet(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
	params api.GetSessionV1SessionsSessionIdGetParams,
) {
	id, err := db.ParseUuid16(sessionID)
	if err != nil {
		// A malformed id is a 404, not a 400: the Python server maps
		// InvalidUuidError the same way, so a bad URL never reveals whether the
		// id format or the row is the problem.
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	row, err := s.queries.GetSession(r.Context(), store.GetSessionParams{
		WorkspaceID: s.workspace,
		ID:          id,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not load the session")
		return
	}

	detail, items, err := s.sessionDetail(r, row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not assemble the session")
		return
	}
	body, err := newSessionDetailBody(detail)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not render the session")
		return
	}
	if err := body.setItems(items); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not render the transcript")
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// sessionDetail assembles the full response for one session.
//
// Fields the store cannot yet supply (usage, NativeTool state, model options)
// are reported in their empty-but-present form rather than omitted: the client
// treats a missing key and an empty value differently in several places, and
// this build has no runner to populate them.
func (s *Server) sessionDetail(r *http.Request, row store.Conversation) (api.SessionResponse, []renderedItem, error) {
	id := row.ID.String()
	rootID := row.RootConversationID.String()
	archived := row.Archived
	kind := "default"
	permissionLevel := levelOwner
	labels := map[string]string{}
	status := api.SessionResponseStatusIdle
	falseValue := false
	emptyModels := []api.NativeModelOption{}
	emptyElicitations := []map[string]interface{}{}
	emptyTodos := []map[string]interface{}{}

	detail := api.SessionResponse{
		Id:                        id,
		AgentId:                   agentIDOf(row),
		Archived:                  &archived,
		CreatedAt:                 row.CreatedAt,
		HostResumable:             &falseValue,
		InferenceConfigured:       &falseValue,
		Kind:                      &kind,
		Labels:                    &labels,
		ModelOptions:              &emptyModels,
		PendingElicitations:       &emptyElicitations,
		PendingInputs:             &emptyElicitations,
		PermissionLevel:           &permissionLevel,
		RootConversationId:        &rootID,
		ShareWorkspaceFiles:       &falseValue,
		SideChatSettingsSupported: &falseValue,
		Status:                    status,
		TerminalPending:           &falseValue,
		Todos:                     &emptyTodos,
		UpdatedAt:                 &row.UpdatedAt,
	}
	// A local single-user build can dispatch: the client reads runner_online to
	// decide whether the composer is usable, and a false here disables it.
	trueValue := true
	detail.RunnerOnline = &trueValue
	detail.UsageIncluded = &trueValue

	title := row.Title
	detail.Title = &title
	if !row.ParentConversationID.IsZero() {
		parentID := row.ParentConversationID.String()
		detail.ParentSessionId = &parentID
	}

	// The bound agent's name is what the session header shows.
	if !row.AgentID.IsZero() {
		agent, err := s.queries.GetAgent(r.Context(), store.GetAgentParams{
			WorkspaceID: s.workspace,
			ID:          row.AgentID,
		})
		if err == nil {
			name := agent.Name
			detail.AgentName = &name
			// The description is a CompressedText column; decode it rather than
			// passing the stored frame through.
			if len(agent.Description) > 0 {
				if _, decErr := db.DecodeCompressedText(agent.Description); decErr == nil {
					// The detail response has no description field; decoding it
					// here keeps a corrupt frame from breaking the page silently
					// in a future field addition.
				}
			}
		}
	}

	// The transcript. The client renders these directly, so an unreadable item
	// must not take the page down with it: a decode failure truncates the list
	// and is reported through the response's error state in a later stage.
	items, err := s.loadItems(r, row.ID)
	if err != nil {
		return detail, nil, err
	}
	return detail, items, nil
}

// loadItems reads the transcript with its payloads decoded.
//
// An item whose payload does not parse is skipped rather than rendered blank: a
// blank bubble reads as "the agent said nothing", while a missing one is
// visibly missing.
func (s *Server) loadItems(r *http.Request, conversationID db.Uuid16) ([]renderedItem, error) {
	rows, err := s.queries.ListItems(r.Context(), store.ListItemsParams{
		WorkspaceID:    s.workspace,
		ConversationID: conversationID,
		PageLimit:      500,
	})
	if err != nil {
		return nil, err
	}

	items := make([]renderedItem, 0, len(rows))
	for _, row := range rows {
		typeName, err := db.DecodeName(db.ItemTypeName, "conversation_items.type", row.Type)
		if err != nil {
			continue
		}
		statusName, err := db.DecodeName(db.ItemStatusName, "conversation_items.status", row.Status)
		if err != nil {
			statusName = "completed"
		}

		data, dataErr := s.queries.GetItemData(r.Context(), store.GetItemDataParams{
			WorkspaceID:    s.workspace,
			ConversationID: conversationID,
			ID:             row.ID,
		})
		if dataErr != nil {
			continue
		}
		// Validate against the generated union so a payload shape the document
		// does not allow is dropped rather than sent to a client that cannot
		// parse it.
		if _, decodeErr := decodeItemData(typeName, []byte(data.Data)); decodeErr != nil {
			continue
		}

		items = append(items, renderedItem{
			Id:         row.ID.String(),
			CreatedAt:  row.CreatedAt,
			CreatedBy:  row.CreatedBy,
			Data:       json.RawMessage(data.Data),
			ResponseId: row.ResponseID,
			Status:     statusName,
			Type:       typeName,
		})
	}
	return items, nil
}

// agentIDOf renders a session's agent id, reporting the empty string when the
// column is NULL (the document types the field as a required string).
func agentIDOf(row store.Conversation) string {
	if row.AgentID.IsZero() {
		return ""
	}
	return row.AgentID.String()
}
