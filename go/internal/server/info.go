package server

// The /v1/info boot handshake.
//
// The web client reads this before rendering anything: the flags decide which
// affordances exist (agent install, automations, fork, pull requests) and
// branding decides the page chrome. A local single-user install reports a
// specific set of values, and a Go build must serve the same page rather than
// one with features silently switched off.
//
// The response ships as a literal rather than through the generated struct
// because ServerInfoResponse marks most flags optional-and-omitempty while the
// Python server emits all of them; the literal is verified against both the
// document's required fields and a live Python response by info_test.go.

import (
	"encoding/json"
	"net/http"
)

// infoTemplate is the response body for a local single-user install, with the
// build version substituted.
//
// Captured from a live Python server: agent management and the session features
// it implies are available; managed sandboxes, Databricks, smart routing, and
// dictation are not. Guessing at these would either hide working UI or show UI
// that then fails.
const infoTemplate = `{
  "accounts_enabled": false,
  "agent_config_edit": true,
  "agent_detail": true,
  "agent_install": true,
  "archive_worktree_cleanup": true,
  "automations_enabled": true,
  "branding": {
    "app_name": null,
    "heading": null,
    "logos": {
      "favicon": null,
      "loading": null,
      "main": null
    },
    "powered_by": true
  },
  "databricks_features": false,
  "dictation_available": false,
  "durable_initial_input": false,
  "enabled_connections": [],
  "features": {
    "arca_shutdown_warnings": false,
    "automations_v2": false,
    "canvas": false,
    "codex_plan_at_launch": true,
    "custom_agents_settings_ui": false,
    "grid_sessions_display_ui": false,
    "harness_install": false,
    "import_review": false,
    "sandbox_profiles": false,
    "session_orchestration": true,
    "sidebar_grouping_ui": false,
    "splittable_terminal_view_ui": false,
    "usage_page": false,
    "workspace_content_search": false
  },
  "filesystem_attachments": {
    "all_types": false,
    "extensions": [
      ".db",
      ".docx",
      ".pptx",
      ".sqlite",
      ".sqlite3",
      ".xlsx",
      ".zip"
    ]
  },
  "harness_install_enabled": false,
  "installable_harnesses": [],
  "login_url": null,
  "managed_sandboxes_enabled": false,
  "needs_setup": false,
  "picker": {
    "default_agent_name": null,
    "restrict_to_agent_names": []
  },
  "public_sharing_enabled": true,
  "public_sharing_max_level": "read",
  "request_managed_sandbox_host_id": true,
  "sandbox_provider": null,
  "sandbox_provider_capabilities": {},
  "sandbox_providers": [],
  "scheduled_task_projects": true,
  "scheduled_task_session_modes": true,
  "server_version": "",
  "session_event_types": [
    "approval",
    "clear",
    "compact",
    "compaction",
    "error",
    "external_btw_dismiss",
    "function_call",
    "function_call_output",
    "interrupt",
    "mcp_elicitation",
    "message",
    "native_tool",
    "reasoning",
    "resource_event",
    "retry_session",
    "routing_decision",
    "slash_command",
    "stop_session",
    "terminal_command"
  ],
  "session_fork_workspaces": true,
  "session_pull_requests": true,
  "session_search_scope": true,
  "sharing_mode": "on",
  "single_user": true,
  "smart_routing_enabled": false,
  "smart_routing_sources": {
    "external": false,
    "oss": false
  }
}`

// InfoV1InfoGet implements GET /v1/info: the client's boot handshake.
func (s *Server) InfoV1InfoGet(w http.ResponseWriter, r *http.Request) {
	body := make(map[string]json.RawMessage)
	if err := json.Unmarshal([]byte(infoTemplate), &body); err != nil {
		// A malformed template is a build-time mistake, not a runtime one; the
		// handshake test parses it so this cannot ship.
		writeError(w, http.StatusInternalServerError, "internal_error",
			"server info is misconfigured")
		return
	}
	version, err := json.Marshal(s.version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"server info is misconfigured")
		return
	}
	body["server_version"] = version
	writeJSON(w, http.StatusOK, body)
}
