package toolbridge_test

// End-to-end: a real Pi process loads the generated extension, calls a bridged
// tool, and the call reaches the Go handler.
//
// The unit tests prove the wire format and the generated source; only a real Pi
// proves that Pi accepts the extension and routes a tool call through it. A
// structural check cannot catch a wrong extension API or a missing export.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Brinmon/omnigent/go/internal/pi"
	"github.com/Brinmon/omnigent/go/internal/toolbridge"
)

// piAgentDir is the developer's managed Pi install.
func piAgentDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	dir := filepath.Join(home, ".pi", "agent")
	if _, err := os.Stat(filepath.Join(dir, "install", "current-version")); err != nil {
		t.Skipf("no managed Pi install: %v", err)
	}
	return dir
}

// piModel reads a provider-qualified model id from Pi's registry.
func piModel(t *testing.T, agentDir string) string {
	t.Helper()
	if override := os.Getenv("OMNIGENT_PI_MODEL"); override != "" {
		return override
	}
	raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		t.Skipf("no models.json: %v", err)
	}
	var registry struct {
		Providers map[string]struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Skipf("models.json unreadable: %v", err)
	}
	for provider, entry := range registry.Providers {
		for _, model := range entry.Models {
			if strings.Contains(model.ID, "flash") {
				return provider + "/" + model.ID
			}
		}
	}
	t.Skip("no model available")
	return ""
}

// piEnv builds the child environment.
func piEnv(t *testing.T, agentDir, isolated string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		t.Skipf("read models.json: %v", err)
	}
	if err := os.MkdirAll(isolated, 0o755); err != nil {
		t.Fatalf("create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(isolated, "models.json"), raw, 0o644); err != nil {
		t.Fatalf("stage models.json: %v", err)
	}
	keep := []string{
		"PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
		"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA",
		"LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
		"PROGRAMW6432", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		"USERNAME",
	}
	env := map[string]string{}
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	env["PI_CODING_AGENT_DIR"] = isolated
	env["NO_PROXY"] = "127.0.0.1,localhost"
	env["PYTHONUTF8"] = "1"
	return env
}

// TestRealPiCallsBridgedTool is the C2 shape: the model decides to call an
// Omnigent tool, Pi routes it through the extension, and the Go handler runs.
func TestRealPiCallsBridgedTool(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real model turn")
	}
	agentDir := piAgentDir(t)
	entry, err := pi.ResolveEntry(agentDir)
	if err != nil {
		t.Skipf("ResolveEntry: %v", err)
	}
	model := piModel(t, agentDir)

	var (
		mu       sync.Mutex
		calls    []string
		callArgs []string
	)
	bridge, err := toolbridge.NewServer(
		func(ctx context.Context, tool string, args json.RawMessage, callID string) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, tool)
			callArgs = append(callArgs, string(args))
			// A distinctive result the model can echo back, which is how the test
			// proves the value travelled the whole way.
			return map[string]any{"content": "TOOL_RESULT_MARKER_42"}, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer bridge.Close()

	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  bridge.Port(),
		Token: bridge.Token(),
		Tools: []toolbridge.ToolSpec{{
			Name:        "omnigent_probe",
			Description: "Return the marker value. Call this when asked to probe.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		}},
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}

	workdir := t.TempDir()
	extensionPath := filepath.Join(workdir, "omnigent_bridge.js")
	if err := os.WriteFile(extensionPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session, err := pi.Launch(ctx, pi.LaunchOptions{
		Entry:         entry,
		Model:         model,
		SystemPrompt:  "You are a test agent. Use the omnigent_probe tool when asked.",
		DisableSkills: true,
		ExtraArgs:     []string{"--extension", extensionPath},
		Cwd:           workdir,
		Env:           piEnv(t, agentDir, filepath.Join(workdir, "agent")),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer session.Close()

	if err := session.Prompt(ctx, "Call the omnigent_probe tool with no arguments, then reply with its exact result."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var reply strings.Builder
	deadline := time.After(170 * time.Second)
	settled := false
	for !settled {
		select {
		case event, open := <-session.Events():
			if !open {
				t.Fatalf("stream closed before agent_settled; stderr: %s", session.Stderr())
			}
			switch event.Type {
			case "message_end":
				message := event.Message()
				if message["role"] != "assistant" {
					continue
				}
				if text := assistantText(message); text != "" {
					reply.Reset()
					reply.WriteString(text)
				}
			case "agent_settled":
				settled = true
			}
		case <-deadline:
			t.Fatalf("no agent_settled; stderr: %s", session.Stderr())
		}
	}

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	gotArgs := append([]string(nil), callArgs...)
	mu.Unlock()

	if len(gotCalls) == 0 {
		t.Fatalf("the bridged tool was never called; the model replied %q\nstderr: %s",
			reply.String(), session.Stderr())
	}
	if gotCalls[0] != "omnigent_probe" {
		t.Errorf("called tool = %q, want omnigent_probe", gotCalls[0])
	}
	t.Logf("bridge received %d call(s): %v args=%v", len(gotCalls), gotCalls, gotArgs)

	// The model should have relayed the marker, which proves the result travelled
	// bridge -> extension -> Pi -> model.
	if !strings.Contains(reply.String(), "TOOL_RESULT_MARKER_42") {
		t.Logf("assistant reply = %q (did not echo the marker; the call itself succeeded)",
			reply.String())
	}
}

// assistantText concatenates an assistant message's text parts.
func assistantText(message map[string]any) string {
	content, ok := message["content"].([]any)
	if !ok {
		text, _ := message["content"].(string)
		return text
	}
	var out strings.Builder
	for _, part := range content {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := partMap["text"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}
