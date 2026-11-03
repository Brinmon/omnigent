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
	// The create records the client surface it came from; a session read back
	// must report it or the client loses the label it wrote itself.
	labels := sessionLabels("")
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

	// The workspace comes from the metadata row, not the conversation. The
	// composer reads it to show where the session runs; leaving it unset shows
	// "No workspace" for a session that was created with a directory.
	if metadata, metaErr := s.queries.GetSessionMetadata(r.Context(),
		store.GetSessionMetadataParams{
			WorkspaceID: s.workspace,
			ID:          row.ID,
		}); metaErr == nil {
		detail.Workspace = metadata.Workspace
		detail.GitBranch = metadata.GitBranch
		detail.HostId = uuidValue(metadata.HostID)
		detail.ProjectId = uuidValue(metadata.ProjectID)
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

			// harness, llm_model, and context_window are derived from the bundle
			// rather than stored, so they are absent unless the blob is read.
			if spec, ok := s.readAgentBundle(r.Context(), agent.BundleLocation); ok {
				if spec.Harness != "" {
					harness := spec.Harness
					detail.Harness = &harness
				}
				if spec.Model != "" {
					model := spec.Model
					detail.LlmModel = &model
					detail.ContextWindow = contextWindowFor(model)
				}
			}
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

// uuidValue renders a uuid column as the bare hex string the client expects,
// or nil when the column is the zero value (stored as NULL).
func uuidValue(id db.Uuid16) *string {
	if id.IsZero() {
		return nil
	}
	text := id.String()
	return &text
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

// ListSessionItemsV1SessionsSessionIdItemsGet implements GET .../items.
//
// The client reads the transcript from here rather than from the session detail's
// “items“ field, so an unimplemented route leaves the session page showing
// "Conversation not found" even though the session and its rows exist.
//
// The response is a PaginatedList whose “data“ holds ConversationItem objects.
func (s *Server) ListSessionItemsV1SessionsSessionIdItemsGet(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
	params api.ListSessionItemsV1SessionsSessionIdItemsGetParams,
) {
	id, err := db.ParseUuid16(sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}
	if _, err := s.queries.GetSession(r.Context(), querySession(s.workspace, id)); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Session not found.")
		return
	}

	limit := int64(100)
	if params.Limit != nil {
		limit = int64(*params.Limit)
	}
	rows, err := s.queries.ListItems(r.Context(), store.ListItemsParams{
		WorkspaceID:    s.workspace,
		ConversationID: id,
		PageLimit:      limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not load the transcript")
		return
	}

	items := make([]any, 0, len(rows))
	for _, row := range rows {
		view, ok := s.itemView(r, id, row)
		if !ok {
			continue
		}
		items = append(items, view)
	}

	object := "list"
	hasMore := false
	var firstID, lastID *string
	if len(items) > 0 {
		firstItem, _ := items[0].(map[string]any)
		lastItem, _ := items[len(items)-1].(map[string]any)
		if id, ok := firstItem["id"].(string); ok {
			firstID = &id
		}
		if id, ok := lastItem["id"].(string); ok {
			lastID = &id
		}
	}
	writeJSON(w, http.StatusOK, api.PaginatedList{
		Object:  &object,
		Data:    &items,
		FirstId: firstID,
		LastId:  lastID,
		HasMore: &hasMore,
	})
}

// itemView renders one stored row as the client's item shape.
//
// The payload is FLATTENED into the envelope, not nested under "data": the Python
// server's response puts a compaction's summary and token_count at the top level
// beside id and type, and the client reads them there. Nesting them leaves the
// client unable to find the fields it expects.
//
// A row whose payload does not decode is skipped rather than sent half-formed,
// since the client renders each item against its own type.
func (s *Server) itemView(
	r *http.Request,
	conversationID db.Uuid16,
	row store.ListItemsRow,
) (map[string]any, bool) {
	typeName, err := db.DecodeName(db.ItemTypeName, "conversation_items.type", row.Type)
	if err != nil {
		return nil, false
	}
	statusName, err := db.DecodeName(db.ItemStatusName, "conversation_items.status", row.Status)
	if err != nil {
		statusName = "completed"
	}

	data, err := s.queries.GetItemData(r.Context(), store.GetItemDataParams{
		WorkspaceID:    s.workspace,
		ConversationID: conversationID,
		ID:             row.ID,
	})
	if err != nil {
		return nil, false
	}
	payload, err := decodeItemData(typeName, []byte(data.Data))
	if err != nil {
		return nil, false
	}
	// The union has no generic setter, so the payload is re-marshalled and merged
	// into the envelope.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, false
	}

	view := map[string]any{
		"id":          row.ID.String(),
		"response_id": row.ResponseID,
		"type":        typeName,
		"status":      statusName,
		"created_at":  row.CreatedAt,
	}
	// The envelope's own fields win over a payload that happens to carry one: id
	// and type identify the row, and a payload must not be able to rewrite them.
	for key, value := range fields {
		switch key {
		case "id", "type", "status", "created_at", "response_id", "created_by":
			continue
		}
		view[key] = value
	}
	if row.CreatedBy != nil {
		view["created_by"] = *row.CreatedBy
	}
	return view, true
}
