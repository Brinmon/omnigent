package server

import (
	"encoding/hex"
	"net/http"

	"github.com/Brinmon/omnigent/go/internal/api"
)

// ListHostWorktreesV1HostsHostIdWorktreesGet reports the git worktrees the host
// knows about.
//
// The Python server forwards this to a connected runner, which answers 409 when
// offline. This build runs the harness in-process and does not create worktrees,
// so the honest answer is an empty list: the picker then offers the plain
// directory rather than a worktree it cannot open.
func (s *Server) ListHostWorktreesV1HostsHostIdWorktreesGet(
	w http.ResponseWriter, r *http.Request, hostID string,
	params api.ListHostWorktreesV1HostsHostIdWorktreesGetParams) {
	if hex.EncodeToString(localHostID()) != hostID {
		writeError(w, http.StatusNotFound, "not_found", "Host not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     []any{},
		"has_more": false,
	})
}

// GetSkillsV1SkillsGet reports the skills available to an agent.
//
// Empty because this build ships no skill library; the client renders an empty
// picker rather than an error, which is the difference between "none installed"
// and "feature broken".
func (s *Server) GetSkillsV1SkillsGet(
	w http.ResponseWriter, r *http.Request, params api.GetSkillsV1SkillsGetParams) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []any{},
	})
}
