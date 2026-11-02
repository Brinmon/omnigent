package pi

// These tests drive a real ``pi --mode rpc`` process. They are the P3 evidence
// that the Go client speaks Pi's protocol, so they run by default when Pi is
// installed and skip with a clear reason when it is not -- a skipped test here
// means "not verified", never "passed".

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testAgentDir is the managed Pi install the developer already has.
func testAgentDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	dir := filepath.Join(home, ".pi", "agent")
	if _, err := os.Stat(filepath.Join(dir, "install", "current-version")); err != nil {
		t.Skipf("no managed Pi install under %s: %v", dir, err)
	}
	return dir
}

// testModel reads the provider-qualified model id from Pi's own registry, so the
// test does not encode one developer's model choice.
func testModel(t *testing.T, agentDir string) string {
	t.Helper()
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
		t.Skipf("models.json is not readable: %v", err)
	}
	for provider, entry := range registry.Providers {
		for _, model := range entry.Models {
			if strings.Contains(model.ID, "flash") {
				return provider + "/" + model.ID
			}
		}
	}
	t.Skip("no model available in models.json")
	return ""
}

// testEnv builds the child environment from the developer's Pi install.
//
// Pi needs its registry to resolve a model id, so models.json is copied into an
// isolated agent dir: the isolation exists to exclude global packages (a broken
// extension aborts Pi at startup), not to hide the model list.
func testEnv(t *testing.T, agentDir, isolated string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		t.Skipf("read models.json: %v", err)
	}
	if err := os.MkdirAll(isolated, 0o755); err != nil {
		t.Fatalf("create isolated agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(isolated, "models.json"), raw, 0o644); err != nil {
		t.Fatalf("stage models.json: %v", err)
	}

	// Windows cannot start a process with a stripped environment: Winsock init
	// needs SYSTEMROOT and Path.home() needs USERPROFILE.
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

// TestResolveEntryFindsManagedInstall covers the Windows path: the npm “.cmd“
// shim cannot be driven over a pipe, so the real JS entry must be found.
func TestResolveEntryFindsManagedInstall(t *testing.T) {
	agentDir := testAgentDir(t)

	entry, err := ResolveEntry(agentDir)
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	if len(entry) != 2 {
		t.Fatalf("entry = %v, want [node <cli.js>]", entry)
	}
	if filepath.Base(entry[0]) != "node" && !strings.HasSuffix(entry[0], "node.exe") {
		t.Errorf("entry[0] = %q, want node", entry[0])
	}
	if !strings.HasSuffix(entry[1], ".js") {
		t.Errorf("entry[1] = %q, want the JS entry point", entry[1])
	}
	if _, err := os.Stat(entry[1]); err != nil {
		t.Errorf("entry point does not exist: %v", err)
	}
}

// TestLaunchAndPrompt runs one real turn and asserts the protocol shape.
//
// This is the C1-equivalent on the Go side: a real Pi process, a real provider,
// and the turn boundary Pi actually emits.
func TestLaunchAndPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real model turn")
	}
	agentDir := testAgentDir(t)
	entry, err := ResolveEntry(agentDir)
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	model := testModel(t, agentDir)

	workdir := t.TempDir()
	env := testEnv(t, agentDir, filepath.Join(workdir, "agent"))

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session, err := Launch(ctx, LaunchOptions{
		Entry:         entry,
		Model:         model,
		SystemPrompt:  "You are a minimal test agent.",
		DisableSkills: true,
		Cwd:           workdir,
		Env:           env,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer session.Close()

	if err := session.Prompt(ctx, "Reply with exactly the word PONG."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		sawAck       bool
		sawDelta     bool
		sawSettled   bool
		assistantTxt strings.Builder
	)
	deadline := time.After(170 * time.Second)
	for !sawSettled {
		select {
		case event, open := <-session.Events():
			if !open {
				t.Fatalf("stream closed before agent_settled; stderr: %s", session.Stderr())
			}
			switch event.Type {
			case "response":
				// The command acknowledgement carries disposition=started.
				sawAck = true
			case "message_update":
				if delta := event.TextDelta(); delta != "" {
					sawDelta = true
					assistantTxt.WriteString(delta)
				}
			case "message_end":
				message := event.Message()
				if message["role"] != "assistant" {
					continue
				}
				// message_end repeats the full content; use it as the
				// authoritative text rather than trusting the delta sum.
				assistantTxt.Reset()
				assistantTxt.WriteString(assistantText(message))
			case "agent_settled":
				sawSettled = true
			}
		case <-deadline:
			t.Fatalf("no agent_settled within the deadline; stderr: %s", session.Stderr())
		}
	}

	if !sawAck {
		t.Error("no response acknowledgement; Pi did not accept the prompt")
	}
	if !sawDelta {
		t.Error("no text_delta; streaming would not reach the UI")
	}
	if !strings.Contains(assistantTxt.String(), "PONG") {
		t.Errorf("assistant text = %q, want it to contain PONG", assistantTxt.String())
	}
}

// assistantText concatenates the text parts of a message's content.
func assistantText(message map[string]any) string {
	parts, ok := message["content"].([]any)
	if !ok {
		if text, ok := message["content"].(string); ok {
			return text
		}
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
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

// TestAbortSettlesTurn covers interrupt: the C4 shape, where a turn ends early
// and reports itself as aborted rather than completed.
func TestAbortSettlesTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("drives a real model turn")
	}
	agentDir := testAgentDir(t)
	entry, err := ResolveEntry(agentDir)
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	model := testModel(t, agentDir)

	workdir := t.TempDir()
	env := testEnv(t, agentDir, filepath.Join(workdir, "agent"))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	session, err := Launch(ctx, LaunchOptions{
		Entry: entry, Model: model, DisableSkills: true, Cwd: workdir, Env: env,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer session.Close()

	// Start something long, then abort it. The prompt need not finish.
	if err := session.Prompt(ctx, "Count slowly from 1 to 500, one number per line."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := session.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		select {
		case event, open := <-session.Events():
			if !open {
				t.Fatalf("stream closed before agent_settled; stderr: %s", session.Stderr())
			}
			if event.Type != "agent_settled" {
				continue
			}
			// ``aborted`` is how the server distinguishes a user stop from a
			// completed turn; the UI shows a stop badge only for this case.
			aborted, _ := event.Raw["aborted"].(bool)
			if !aborted {
				t.Error("agent_settled reports aborted=false after Abort")
			}
			return
		case <-deadline:
			t.Fatalf("no agent_settled after abort; stderr: %s", session.Stderr())
		}
	}
}
