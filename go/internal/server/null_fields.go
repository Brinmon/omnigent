package server

// Wire shape of an item payload, captured from a live Python server.
//
// The stored column holds the model's field names; the HTTP response renames
// some on the way out (``agent`` becomes ``model``) and emits a specific set of
// optional fields as null. Which fields those are belongs to each model, so the
// table is generated from a real response (designs/go-rewrite/gen-payload-nulls.py)
// rather than from the generated Go struct, whose every optional field is
// ``omitempty`` and would add keys the server never sends.

import (
	"encoding/json"
)

// payloadFieldOrder is each item type's response key set. Order is irrelevant to
// JSON but keeps the table readable.
var payloadFieldOrder = map[string][]string{
	"compaction":           {"compacted_messages", "last_item_id", "model", "summary", "token_count", "window_id"},
	"error":                {"cause", "code", "level", "message", "remediation", "source", "title"},
	"function_call":        {"arguments", "call_id", "model", "name"},
	"function_call_output": {"call_id", "output"},
	"message":              {"content", "model", "role", "stream_message_id"},
	"native_tool":          {"item"},
	"reasoning":            {"content", "encrypted_content", "model", "summary"},
	"resource_event":       {"event_type", "resource", "resource_id", "resource_type"},
	"routing_decision":     {"agent", "applied", "attempted_override", "decision_id", "harness", "model", "rationale", "raw_model", "router_source", "scope", "task_description"},
	"slash_command":        {"arguments", "kind", "model", "name", "output"},
	"terminal_command":     {"input", "kind", "stderr", "stdout"},
}

// payloadAliases renames a stored field to its wire name.
var payloadAliases = map[string]map[string]string{
	"function_call": {"agent": "model"},
	"message":       {"agent": "model"},
	"reasoning":     {"agent": "model"},
	"slash_command": {"agent": "model"},
}

// fillItemNulls rewrites a stored payload into the response shape: stored field
// names become their wire aliases, and every key the server always emits is
// present, using null where the value is absent.
func fillItemNulls(itemType string, payload json.RawMessage) (json.RawMessage, error) {
	keys, ok := payloadFieldOrder[itemType]
	if !ok {
		return payload, nil
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(payload, &stored); err != nil {
		return payload, err
	}

	out := make(map[string]json.RawMessage, len(keys))
	for key, value := range stored {
		if wire, renamed := payloadAliases[itemType][key]; renamed {
			out[wire] = value
			continue
		}
		out[key] = value
	}
	// Any key the server emits but the stored row lacks becomes null.
	for _, key := range keys {
		if _, present := out[key]; !present {
			out[key] = json.RawMessage("null")
		}
	}
	return json.Marshal(out)
}

// sessionResponseFields is SessionResponse's optional key set, generated from
// the struct tags so it follows the document. The Python server emits every
// one of these, sending null where it has no value.
var sessionResponseFields = []string{
	"active_response_id",
	"agent_name",
	"archived",
	"background_task_count",
	"background_tasks",
	"context_window",
	"cost_control_mode_override",
	"external_session_id",
	"git_branch",
	"harness",
	"host_id",
	"host_online",
	"host_resumable",
	"inference_configured",
	"inference_error",
	"items",
	"kind",
	"labels",
	"last_task_error",
	"last_total_tokens",
	"llm_model",
	"mcp_startup",
	"model_options",
	"model_override",
	"native_usage_baseline",
	"parent_session_id",
	"pending_elicitations",
	"pending_inputs",
	"permission_level",
	"project_id",
	"reasoning_effort",
	"root_conversation_id",
	"runner_id",
	"runner_online",
	"sandbox_profile",
	"sandbox_status",
	"share_workspace_files",
	"side_chat_settings_supported",
	"sub_agent_name",
	"subagent_routing_override",
	"terminal_launch_args",
	"terminal_pending",
	"title",
	"todos",
	"total_cost_usd",
	"updated_at",
	"usage_by_model",
	"usage_included",
	"workspace",
}
