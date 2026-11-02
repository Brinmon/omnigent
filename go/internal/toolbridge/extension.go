package toolbridge

// Generating the Pi extension that registers Omnigent's tools and gates native
// ones.
//
// Pi loads this file as an extension at startup. It does two things the bridge
// cannot do from Go:
//
//   - registers each Omnigent tool with Pi, so the model sees it and Pi routes
//     its calls back over the socket;
//   - hooks pi.on("tool_call") to ask for a TOOL_CALL verdict on *native* tools,
//     which run in-process inside Pi and would otherwise escape every guardrail.
//
// Bridged tools are skipped in that hook on purpose: they are already evaluated
// when the call reaches the bridge, and evaluating again would prompt the user
// twice for one call.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolSpec is one tool as Pi should register it.
type ToolSpec struct {
	// Name is the tool name the model calls.
	Name string
	// Description is shown in the model's tool list.
	Description string
	// Parameters is the JSON Schema for the arguments. Pi passes it to the model
	// unchanged, so it must already be a valid object schema.
	Parameters json.RawMessage
}

// ExtensionOptions configures the generated file.
type ExtensionOptions struct {
	// Port is the bridge's loopback port.
	Port int
	// Token is the bridge's bearer token, embedded so the extension can
	// authenticate itself.
	Token string
	// Tools are the tools to register. An empty list still generates the policy
	// hook, which is what gates Pi's native tools.
	Tools []ToolSpec
}

// GenerateExtension renders the extension source.
//
// The template mirrors the Python implementation's behaviour exactly, including
// the settlement guards: a tool call must resolve exactly once, because a promise
// that never settles hangs Pi's agent loop and the turn with it.
func GenerateExtension(opts ExtensionOptions) (string, error) {
	descriptors := make([]map[string]any, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			// Pi needs a concrete name; an unnamed tool would be rejected at load
			// time and take the whole extension with it.
			continue
		}
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema any
		if err := json.Unmarshal(parameters, &schema); err != nil {
			return "", fmt.Errorf("tool %q: parameters are not valid JSON: %w", tool.Name, err)
		}
		snippet := tool.Description
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		descriptors = append(descriptors, map[string]any{
			"name":          tool.Name,
			"description":   tool.Description,
			"promptSnippet": snippet,
			"parameters":    schema,
		})
	}

	toolsJSON, err := json.MarshalIndent(descriptors, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode tool descriptors: %w", err)
	}
	// json.Marshal so the token is correctly escaped for a JS string literal.
	tokenJSON, err := json.Marshal(opts.Token)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}

	return fmt.Sprintf(extensionTemplate,
		opts.Port,
		toolsJSON,
		opts.Port,
		tokenJSON,
	), nil
}

// extensionTemplate is the generated source.
//
// It is a printf template: four verbs, in the order port, tools, port, token.
const extensionTemplate = `// Auto-generated Omnigent tool bridge extension for Pi.
// Connects to the Omnigent TCP tool server on port %d.
const net = require("net");

const TOOLS = %s;
const BRIDGED = new Set(TOOLS.map((t) => t.name));
const PORT = %d;
const TOKEN = %s;

/** Send a tool call request over TCP and return the result. */
function callTool(toolName, args, callId) {
  return new Promise((resolve) => {
    // Settle exactly once. Route every resolve through finish() so a late
    // "close" after a real "data" response cannot clobber the result, and a
    // bare close with no bytes still resolves rather than hanging Pi's loop.
    let settled = false;
    const finish = (v) => { if (!settled) { settled = true; resolve(v); } };
    const errorResult = (text) => ({
      content: [{ type: "text", text: JSON.stringify({ error: text }) }],
      isError: true
    });
    const client = net.createConnection({ port: PORT, host: "127.0.0.1" }, () => {
      const id = Math.random().toString(36).slice(2);
      const frame = { id, token: TOKEN, tool: toolName, args, call_id: callId };
      const req = JSON.stringify(frame) + "\n";
      let buf = "";
      client.on("data", (chunk) => {
        buf += chunk.toString();
        const nl = buf.indexOf("\n");
        if (nl !== -1) {
          try {
            const resp = JSON.parse(buf.slice(0, nl));
            client.end();
            if (resp.error) {
              finish(errorResult(resp.error));
            } else {
              const text = typeof resp.result === "string"
                ? resp.result
                : JSON.stringify(resp.result);
              const isError = resp.result && (resp.result.error || resp.result.blocked);
              finish({ content: [{ type: "text", text }], isError: !!isError });
            }
          } catch (e) {
            client.end();
            finish(errorResult(e.message));
          }
        }
      });
      client.on("error", (err) => {
        finish(errorResult(err.message));
      });
      // A clean FIN with no bytes (the server failed to serialize its result and
      // closed) emits neither "data" nor "error"; without this the promise would
      // hang forever. finish() is a no-op if already settled.
      client.on("close", () => {
        finish(errorResult("tool server closed connection without a response"));
      });
      client.write(req);
    });
    client.on("error", (err) => {
      finish(errorResult(err.message));
    });
  });
}

/**
 * Ask the Omnigent tool server for a TOOL_CALL policy verdict on a native
 * (non-bridged) Pi tool. Resolves to the verdict object { block, reason } or
 * null.
 *
 * Fails open (null) on any transport error: a wedged native tool would break Pi
 * worse than a missed gate, and bridged tools stay gated server-side regardless.
 */
function evalNativePolicy(toolName, args) {
  return new Promise((resolve) => {
    const client = net.createConnection({ port: PORT, host: "127.0.0.1" }, () => {
      const id = Math.random().toString(36).slice(2);
      const frame = { id, token: TOKEN, kind: "policy_eval", tool: toolName, args };
      const req = JSON.stringify(frame) + "\n";
      let buf = "";
      client.on("data", (chunk) => {
        buf += chunk.toString();
        const nl = buf.indexOf("\n");
        if (nl !== -1) {
          client.end();
          try {
            const resp = JSON.parse(buf.slice(0, nl));
            resolve(resp && resp.verdict ? resp.verdict : null);
          } catch (e) {
            resolve(null);
          }
        }
      });
      client.on("error", () => resolve(null));
      client.write(req);
    });
    client.on("error", () => resolve(null));
  });
}

module.exports = function (pi) {
  // Gate native (non-bridged) tool calls through Omnigent policy. Pi's native
  // tools run in-process and never traverse the bridged path, so without this
  // hook they escape all guardrails. Bridged tools ARE evaluated server-side when
  // the call reaches the bridge, so skip them here to avoid double-evaluation
  // (and double ASK prompts).
  pi.on("tool_call", async (event) => {
    if (!event || typeof event.toolName !== "string") return;
    if (BRIDGED.has(event.toolName)) return;
    const verdict = await evalNativePolicy(event.toolName, event.input || {});
    if (verdict && verdict.block) {
      return { block: true, reason: verdict.reason || "blocked by Omnigent policy" };
    }
  });

  for (const tool of TOOLS) {
    // Pi passes tool.parameters directly to the model as JSON Schema, so the
    // Omnigent schema is used as-is without a TypeBox conversion.
    pi.registerTool({
      name: tool.name,
      label: tool.name,
      description: tool.description,
      promptSnippet: tool.promptSnippet || tool.description,
      parameters: tool.parameters || { type: "object", properties: {} },
      async execute(toolCallId, _params, _signal, _onUpdate, _ctx) {
        return callTool(tool.name, _params, toolCallId);
      },
    });
  }
};
`
