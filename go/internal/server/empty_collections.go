package server

import (
	"net/http"
)

// The endpoints in this file answer with empty collections.
//
// None of them is optional for the client: it requests them on every load, and a
// 501 each made the console show a wall of red while the page itself worked. An
// empty answer is the truthful one -- this build has no hosts, no projects and no
// extensions to report -- and it keeps the failure path meaningful for the
// endpoints that genuinely are not implemented.
//
// The shapes come from driving the Python server against the same fixture rather
// than from openapi.json, for the same reason the create route's did: several of
// these routes describe their response loosely enough that a guessed envelope
// ({"data": []} instead of []) would be silently wrong.

// ListHostsV1HostsGet reports the runner hosts that can execute a session.
//
// The list is empty because this build spawns the harness in-process; there is no
// remote host to offer. The client reads this to decide whether the composer is
// usable, so the response must be a list of hosts rather than an error.
func (s *Server) ListHostsV1HostsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"hosts": []any{}})
}

// ListSessionProjectsV1SessionsProjectsGet reports the projects sessions can be
// grouped under. Empty: projects are not implemented.
func (s *Server) ListSessionProjectsV1SessionsProjectsGet(
	w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// GetProjectOrderV1ProjectsOrderGet reports how the client should sort the project
// list. "alphabetical" with no explicit order is what an unconfigured server
// returns.
func (s *Server) GetProjectOrderV1ProjectsOrderGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sort_mode":           "alphabetical",
		"ordered_project_ids": nil,
	})
}

// ListExtensionsV1ExtensionsGet reports installed client extensions.
func (s *Server) ListExtensionsV1ExtensionsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []any{},
	})
}

// ListProjectsV1ProjectsGet reports the projects a session can belong to.
func (s *Server) ListProjectsV1ProjectsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []any{},
	})
}
