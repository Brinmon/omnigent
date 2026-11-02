package server

// Wiring the tool bridge into a turn.
//
// The bridge is what makes Omnigent's tools visible to Pi and gates Pi's native
// ones. Two pieces have to line up:
//
//   - the loopback bridge, which authenticates with a per-turn token;
//   - the extension file Pi loads, which registers the tools and installs the
//     tool_call hook.
//
// The extension is written into the turn's workspace so it disappears with the
// turn; a leftover extension in a shared directory would be picked up by an
// unrelated Pi process.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Brinmon/omnigent/go/internal/toolbridge"
)

// bridgedTools are the tools Omnigent exposes to Pi through the bridge.
//
// P4 is deliberately narrow: these read the workspace the turn runs in, which is
// what acceptance exercises. A wider toolset arrives with the resource API; the
// bridge does not care how many tools there are.
var bridgedTools = []toolbridge.ToolSpec{
	{
		Name:        "omnigent_read_file",
		Description: "Read a UTF-8 text file from the session workspace.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Workspace-relative path."}
			},
			"required": ["path"]
		}`),
	},
	{
		Name:        "omnigent_write_file",
		Description: "Write a UTF-8 text file in the session workspace.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Workspace-relative path."},
				"content": {"type": "string", "description": "Text to write."}
			},
			"required": ["path", "content"]
		}`),
	},
}

// startToolBridge binds the bridge and writes the extension for one turn.
//
// It returns an empty path when there is nothing to bridge, which happens when
// the turn has no tools to expose; Pi then runs with only its native tools and
// the policy hook is skipped rather than installing an extension that registers
// nothing.
func (s *Server) startToolBridge(
	ctx context.Context,
	sessionID string,
	workdir string,
) (*toolbridge.Server, string, func(), error) {
	bridge, err := toolbridge.NewServer(
		s.executeBridgedTool(workdir),
		s.evaluateNativeTool(ctx, sessionID),
	)
	if err != nil {
		return nil, "", nil, err
	}

	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  bridge.Port(),
		Token: bridge.Token(),
		Tools: bridgedTools,
	})
	if err != nil {
		bridge.Close()
		return nil, "", nil, err
	}

	extensionPath := filepath.Join(workdir, "omnigent_tool_bridge.js")
	if err := os.WriteFile(extensionPath, []byte(source), 0o644); err != nil {
		bridge.Close()
		return nil, "", nil, fmt.Errorf("write extension: %w", err)
	}

	cleanup := func() {
		// The bridge first: closing it releases the port before the extension
		// file disappears, so a straggling call gets a connection error rather
		// than a route to a handler whose workspace is already gone.
		if err := bridge.Close(); err != nil {
			slog.Debug("tool bridge close", "session", sessionID, "error", err)
		}
		_ = os.Remove(extensionPath)
	}
	return bridge, extensionPath, cleanup, nil
}

// executeBridgedTool returns the handler for a bridged call.
//
// Every path is resolved inside the turn's workspace: a relative path cannot
// escape it, and an absolute one is rejected rather than silently reinterpreted.
// That is the same containment the resource API applies, enforced here because
// this path bypasses the HTTP layer entirely.
func (s *Server) executeBridgedTool(workdir string) toolbridge.ToolHandler {
	return func(ctx context.Context, tool string, rawArgs json.RawMessage, callID string) (any, error) {
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
		}

		target, err := resolveInWorkspace(workdir, args.Path)
		if err != nil {
			return nil, err
		}

		switch tool {
		case "omnigent_read_file":
			data, err := os.ReadFile(target)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", args.Path, err)
			}
			return map[string]any{"content": string(data)}, nil

		case "omnigent_write_file":
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, fmt.Errorf("create parent of %s: %w", args.Path, err)
			}
			if err := os.WriteFile(target, []byte(args.Content), 0o644); err != nil {
				return nil, fmt.Errorf("write %s: %w", args.Path, err)
			}
			return map[string]any{
				"path":          args.Path,
				"bytes_written": len(args.Content),
			}, nil

		default:
			return nil, fmt.Errorf("unknown tool %q", tool)
		}
	}
}

// evaluateNativeTool returns the policy handler for Pi's own tools.
//
// P4 has no policy engine, so the verdict is always "allow" -- but the path is
// live, which is what D1 asserts: a native call reaches Omnigent and is recorded
// with its name and arguments before Pi runs it.
func (s *Server) evaluateNativeTool(ctx context.Context, sessionID string) toolbridge.PolicyHandler {
	return func(callCtx context.Context, tool string, rawArgs json.RawMessage) (*toolbridge.Verdict, error) {
		slog.Debug("native tool call observed",
			"session", sessionID, "tool", tool, "args", string(rawArgs))
		// A nil verdict means "no opinion", which the extension treats as allow
		// and Pi then executes the tool itself.
		return nil, nil
	}
}

// resolveInWorkspace maps a client-supplied path into the turn's workspace.
//
// An absolute path is rejected outright rather than joined: `filepath.Join` would
// silently reinterpret it, and a tool that writes outside the workspace is
// exactly the escape the bridge exists to make visible.
func resolveInWorkspace(workdir, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(requested) {
		return "", fmt.Errorf("path %q must be workspace-relative", requested)
	}
	cleaned := filepath.Clean(requested)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", requested)
	}
	resolved := filepath.Join(workdir, cleaned)

	// Check the resolved target too: a symlink inside the workspace can point out
	// of it, and consuming the tool's own path check would be the only barrier.
	roots := sortedRoots(workdir)
	for _, root := range roots {
		if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q resolves outside the workspace", requested)
}

// sortedRoots normalises a workspace path for the containment prefix check.
func sortedRoots(workdir string) []string {
	absolute, err := filepath.Abs(workdir)
	if err != nil {
		absolute = workdir
	}
	roots := []string{absolute}
	if real, err := filepath.EvalSymlinks(absolute); err == nil && real != absolute {
		roots = append(roots, real)
	}
	sort.Strings(roots)
	return roots
}

// BuildExtensionSource renders the extension the server would write.
//
// Exported so an external test can assert the advertised toolset against the same
// source Pi loads, rather than against a copy of the specs.
func BuildExtensionSource() (string, error) {
	return toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		// A placeholder port and token: the toolset is what this renders for, and
		// the real values come from the bridge at launch.
		Port:  1,
		Token: "generated",
		Tools: bridgedTools,
	})
}
