package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Brinmon/omnigent/go/internal/bundle"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// maxBundleUpload bounds the request body. Agent bundles are kilobytes; this is
// generous while keeping an upload from filling memory.
const maxBundleUpload = 32 << 20

// multipartMetadata is the metadata part the client sends beside the bundle.
type multipartMetadata struct {
	Title string `json:"title"`
}

// CreateSessionV1SessionsPost creates a session.
//
// The route dispatches on Content-Type, matching the Python server:
//   - multipart/form-data uploads a bundle and creates the agent and session in
//     one transaction, answering with CreatedSessionResponse (three fields).
//   - application/json binds to an already-registered agent by agent_id and
//     answers with the full SessionResponse.
//
// openapi.json declares neither body nor response for this route (requestBody is
// empty and the 201 schema is {}), so this shape comes from driving the Python
// server rather than from a contract. compare-create.py is the guard.
func (s *Server) CreateSessionV1SessionsPost(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = contentType[:semi]
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	if contentType == "multipart/form-data" {
		s.createSessionFromUpload(w, r)
		return
	}
	s.createSessionFromJSON(w, r)
}

// createSessionFromUpload handles the bundled create the web client uses.
func (s *Server) createSessionFromUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBundleUpload)
	if err := r.ParseMultipartForm(maxBundleUpload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"could not read the multipart body: "+err.Error())
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	// metadata is a plain form value and bundle is the only file part. Reading
	// metadata with FormFile would answer 422 for a request the client sends
	// correctly, which is exactly what the first run of compare-create.py
	// reported.
	var missing []map[string]any
	metadataRaw := r.FormValue("metadata")
	if _, present := r.MultipartForm.Value["metadata"]; !present {
		missing = append(missing, missingField("metadata"))
	}
	bundleFile, _, bundleErr := r.FormFile("bundle")
	if bundleErr != nil {
		missing = append(missing, missingField("bundle"))
	}
	if len(missing) > 0 {
		writeValidationError(w, missing)
		return
	}
	defer func() { _ = bundleFile.Close() }()

	metadataBytes := []byte(metadataRaw)

	var metadata multipartMetadata
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input",
				"metadata is not valid JSON: "+err.Error())
			return
		}
	}

	payload, err := io.ReadAll(io.LimitReader(bundleFile, maxBundleUpload+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"could not read the bundle part: "+err.Error())
		return
	}

	spec, err := bundle.Parse(payload)
	if err != nil {
		// An unreadable or unsupported bundle is the uploader's problem to fix.
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}

	created, err := s.createSessionFromBundle(r.Context(), spec, payload, metadata.Title)
	if err != nil {
		if errors.Is(err, errNoArtifactStore) {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"bundle storage is not configured on this server")
			return
		}
		slog.Error("create session from bundle", "error", err, "agent", spec.Name)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not create the session")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"agent_id":   created.AgentID,
		"agent_name": created.AgentName,
		"session_id": created.SessionID,
	})
}

// createSessionFromJSON handles the bind-to-existing-agent create.
func (s *Server) createSessionFromJSON(w http.ResponseWriter, r *http.Request) {
	// The client sends initial_items (and, on the sub-agent path, parent_session_id
	// and sub_agent_name). Unknown fields must not be rejected: the body belongs to
	// a route openapi.json does not describe, so the client is free to send more
	// than this build reads, and refusing them would break the create.
	var body struct {
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
		// Labels carry the client's create token, which is how it recognises the
		// session it just asked for. Dropping them leaves the client waiting for a
		// row it can never match, and the URL stays on the optimistic temp id.
		Labels map[string]string `json:"labels"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		// A body that is not JSON at all is reported as a shape problem on
		// agent_id, which is the field the client must supply.
		writeValidationError(w, []map[string]any{missingField("agent_id")})
		return
	}
	if body.AgentID == "" {
		writeValidationError(w, []map[string]any{missingField("agent_id")})
		return
	}

	// A malformed id answers 404 rather than 400, matching GetSession: the
	// response must not reveal whether the format or the row was wrong.
	agentID, err := db.ParseUuid16(body.AgentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	agent, err := s.queries.GetAgent(r.Context(), store.GetAgentParams{
		WorkspaceID: s.workspace,
		ID:          agentID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"Agent not found: '"+body.AgentID+"'")
		return
	}

	surface := clientSurfaceOf(r.Header.Get("X-Omnigent-Client"))
	sessionID, err := s.createSessionForAgent(
		r.Context(), agentID, agent.Name, body.Title, surface, body.Labels)
	if err != nil {
		slog.Error("create session for agent", "error", err, "agent", body.AgentID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not create the session")
		return
	}

	// The client expects the full snapshot here, so it is read back through the
	// same path a later GET uses rather than assembled a second time: two
	// construction sites would drift and only one would be noticed.
	row, err := s.queries.GetSession(r.Context(), querySession(s.workspace, sessionID))
	if err != nil {
		slog.Error("read back created session", "error", err, "session", sessionID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"the session was created but could not be read back")
		return
	}
	detail, items, err := s.sessionDetail(r, row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not assemble the session")
		return
	}
	response, err := newSessionDetailBody(detail)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not render the session")
		return
	}
	if err := response.setItems(items); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not render the transcript")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

// missingField builds the 422 detail entry FastAPI produces for an absent field.
func missingField(name string) map[string]any {
	return map[string]any{
		"type": "missing",
		// The Python route takes the body as a raw Request and parses it itself,
		// so its validation errors name the field without FastAPI's "body"
		// prefix. Adding the prefix would report the same failure differently.
		"loc":   []any{name},
		"msg":   "Field required",
		"input": map[string]any{},
	}
}

// writeValidationError writes the 422 envelope FastAPI uses.
func writeValidationError(w http.ResponseWriter, detail []map[string]any) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": detail})
}
