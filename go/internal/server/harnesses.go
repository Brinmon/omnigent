package server

// The harness capability matrix.
//
// GET /v1/harnesses is how the web client decides which UI affordances to show:
// an effort picker appears when ``effort`` is not "none", an interrupt button
// when ``interrupt`` is true, and the setup card reads ``setup_steps``. The
// values below are copied from the Python server's own response for ``pi`` so
// the client renders identically, not merely plausibly.
//
// V1 supports exactly one harness. Adding another is a data change here rather
// than a new branch in the server: nothing above this table inspects the
// harness name.

import (
	"net/http"
)

// piCapabilities is Pi's capability matrix, matching the Python server verbatim.
//
// “effort“ is "pi" rather than a level list because Pi manages its own
// thinking levels; “integration_mode“ is "cli-subprocess" because the harness
// runs “pi --mode rpc“ as a child process.
var piCapabilities = map[string]any{
	"auth":                 "omnigent-credential",
	"compaction":           nil,
	"effort":               "pi",
	"elicitation":          "none",
	"fork_history":         "none",
	"images":               nil,
	"instruction_delivery": "composed-per-turn",
	"integration_mode":     "cli-subprocess",
	"interrupt":            true,
	"live_queue":           nil,
	"model_family":         "multi",
	"resume":               "cold-only",
	"shell_tool_name":      nil,
	"shell_tool_prompt":    nil,
	"steering":             nil,
	"streaming":            true,
	"subagents":            false,
}

// piSetupSteps mirrors the Python server's setup cards for Pi.
var piSetupSteps = []map[string]any{
	{
		"action":     "install",
		"command":    nil,
		"detail":     "We'll install Pi on the host for you.",
		"kind":       "install",
		"status_key": "installed",
		"title":      "Install Pi",
	},
	{
		"action":     "auth",
		"command":    nil,
		"detail":     "Add an API key or a gateway so Pi can run.",
		"kind":       "auth",
		"status_key": "authed",
		"title":      "Set up authentication",
	},
}

// ListHarnessesV1HarnessesGet implements GET /v1/harnesses.
//
// The response is the documented “{data, setup_steps}“ envelope. The client
// dereferences “data“ unconditionally, so returning an empty list without the
// array would break boot.
func (s *Server) ListHarnessesV1HarnessesGet(w http.ResponseWriter, r *http.Request) {
	entries := []map[string]any{
		{
			"id":           "pi",
			"label":        "Pi",
			"capabilities": piCapabilities,
			"setup_steps":  piSetupSteps,
		},
	}
	// Top-level setup_steps is keyed by harness for the harnesses that need it;
	// Pi's live per-entry, matching the Python shape.
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        entries,
		"setup_steps": map[string]any{},
	})
}
