// Package pi speaks the “pi --mode rpc“ protocol.
//
// Pi runs as a child process and talks JSONL on stdin/stdout: a command per
// line in, an event per line out. This package owns the process lifecycle and
// the framing; it deliberately does not know what an Omnigent turn is.
//
// Two details of the protocol are easy to get wrong and are handled here:
//
//   - “agent_end“ is not the turn boundary. Pi emits it per agent loop, and an
//     auto-retry produces another one, so a client that stops at “agent_end“
//     truncates a retried turn. “agent_settled“ is the boundary (Pi >= 0.80.4).
//   - on Windows “pi“ resolves to an npm “.cmd“ shim, and a “.cmd“ cannot
//     be driven over a pipe: writing to its stdin fails with EINVAL because
//     cmd.exe does not forward the handle. The managed install also exposes the
//     real JS entry point, so the launcher is resolved explicitly.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event is one JSONL line from Pi.
//
// Only the fields this build reads are typed; the rest stay in Raw so a newer
// Pi adding fields cannot break decoding.
type Event struct {
	Type string
	Raw  map[string]any
}

// String field accessors, kept small so callers do not repeat type assertions.
func (e Event) str(key string) string {
	value, _ := e.Raw[key].(string)
	return value
}

// Message returns the “message“ object of a message event.
func (e Event) Message() map[string]any {
	message, _ := e.Raw["message"].(map[string]any)
	return message
}

// AssistantMessageEvent returns the nested delta object of a message_update.
func (e Event) AssistantMessageEvent() map[string]any {
	inner, _ := e.Raw["assistantMessageEvent"].(map[string]any)
	return inner
}

// TextDelta returns the incremental text of a message_update, or "" for any
// other event. Only “text_delta“ carries user-visible text; “text_start“ and
// “text_end“ bracket it and have no content of their own.
func (e Event) TextDelta() string {
	if e.Type != "message_update" {
		return ""
	}
	inner := e.AssistantMessageEvent()
	if inner["type"] != "text_delta" {
		return ""
	}
	delta, _ := inner["delta"].(string)
	return delta
}

// Session is a running Pi process.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event
	stderr *strings.Builder

	closeOnce sync.Once
	waitErr   error
}

// LaunchOptions configures one Pi process.
type LaunchOptions struct {
	// Entry is the argv prefix that runs Pi (e.g. ["node", "/path/cli.js"]).
	Entry []string
	// Model is the provider-qualified model id, e.g. "openai/deepseek-v4.1-flash".
	Model string
	// Thinking is Pi's own level vocabulary (off/minimal/.../max); empty omits
	// the flag so Pi's model default applies.
	Thinking string
	// SystemPrompt replaces Pi's authored instructions; SystemPromptAppend
	// adds to them. Both may be empty.
	SystemPrompt       string
	SystemPromptAppend string
	// DisableSkills suppresses Pi's skill discovery, which keeps a run from
	// picking up whatever the host has installed.
	DisableSkills bool
	// Cwd is the working directory the harness sees.
	Cwd string
	// Env is the complete child environment. It is not merged with the parent:
	// Pi's behaviour depends on which variables are present, and an inherited
	// environment makes a run depend on the developer's shell.
	Env map[string]string
}

// Launch starts Pi and returns a Session whose Events channel yields parsed
// events until Pi exits.
func Launch(ctx context.Context, opts LaunchOptions) (*Session, error) {
	if len(opts.Entry) == 0 {
		return nil, errors.New("pi: no entry point configured")
	}
	args := append([]string{}, opts.Entry[1:]...)
	args = append(args, "--mode", "rpc", "--no-session")
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Thinking != "" {
		args = append(args, "--thinking", opts.Thinking)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if opts.DisableSkills {
		args = append(args, "--no-skills")
	}
	// An empty append prompt is meaningful: Pi then does not load its bundled
	// APPEND_SYSTEM.md, which is how a caller suppresses Pi's own instructions
	// while keeping the system prompt.
	if opts.SystemPromptAppend != "" {
		args = append(args, "--append-system-prompt", opts.SystemPromptAppend)
	} else if opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", "")
	}

	cmd := exec.CommandContext(ctx, opts.Entry[0], args...)
	cmd.Dir = opts.Cwd
	cmd.Env = flattenEnv(opts.Env)
	// On Windows a console window per child would flash for every turn.
	hideWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pi: start: %w", err)
	}

	session := &Session{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan Event, 64),
		stderr: &strings.Builder{},
	}
	go session.readStdout(stdout)
	go session.readStderr(stderrPipe)
	return session, nil
}

// Events is the parsed event stream. It closes when Pi exits.
func (s *Session) Events() <-chan Event { return s.events }

// Stderr returns whatever Pi wrote to stderr, for diagnostics.
func (s *Session) Stderr() string {
	s.closeOnce.Do(func() {})
	return s.stderr.String()
}

// Prompt sends one user message.
func (s *Session) Prompt(ctx context.Context, text string) error {
	return s.send(map[string]any{"type": "prompt", "message": text})
}

// Steer redirects the running turn without ending it.
func (s *Session) Steer(ctx context.Context, text string) error {
	return s.send(map[string]any{"type": "steer", "message": text})
}

// Abort cancels the running turn. Pi answers with agent_settled{aborted:true}.
func (s *Session) Abort(ctx context.Context) error {
	return s.send(map[string]any{"type": "abort"})
}

// SetThinkingLevel changes Pi's thinking level for subsequent turns.
func (s *Session) SetThinkingLevel(ctx context.Context, level string) error {
	return s.send(map[string]any{"type": "set_thinking_level", "level": level})
}

// send writes one JSONL command.
func (s *Session) send(command map[string]any) error {
	encoded, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("pi: encode command: %w", err)
	}
	// One write per command: Pi reads lines, and a partial line would be a
	// protocol error rather than a short read.
	if _, err := s.stdin.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("pi: write command: %w", err)
	}
	return nil
}

// Close terminates Pi and waits for it to exit.
func (s *Session) Close() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		// Ask first: a SIGKILL leaves Pi unable to flush its own state.
		_ = s.cmd.Process.Signal(os.Interrupt)
	}
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		s.waitErr = err
	case <-time.After(5 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		s.waitErr = <-done
	}
	return s.waitErr
}

// readStdout parses JSONL until EOF.
//
// A line that does not parse is skipped rather than ending the stream: Pi writes
// to stderr for diagnostics and to stdout for protocol, but a stray non-JSON
// line should not truncate a turn.
func (s *Session) readStdout(stdout io.ReadCloser) {
	defer close(s.events)
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	// A single message_end can carry a whole transcript, so the default 64 KiB
	// line limit is far too small.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		eventType, _ := payload["type"].(string)
		s.events <- Event{Type: eventType, Raw: payload}
	}
}

// readStderr drains stderr so Pi cannot block on a full pipe.
func (s *Session) readStderr(stderr io.ReadCloser) {
	defer stderr.Close()
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	for scanner.Scan() {
		s.stderr.WriteString(scanner.Text())
		s.stderr.WriteString("\n")
	}
}

// flattenEnv renders a map as the KEY=value slice exec expects, sorted so a log
// of the child environment is stable.
func flattenEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	// A stable order keeps a failure's environment dump comparable between runs.
	sortStrings(keys)
	flattened := make([]string, 0, len(keys))
	for _, key := range keys {
		flattened = append(flattened, key+"="+env[key])
	}
	return flattened
}

// sortStrings is a tiny insertion sort: the environment has few keys, and this
// keeps the package free of an import for one call.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// ResolveEntry finds the argv that runs Pi.
//
// The managed install records its current release, and its package.json names the
// real entry point; using that avoids the npm “.cmd“ shim, which cannot be
// driven over a pipe on Windows. A native “pi“ on PATH is used as a fallback
// for installs that ship one.
func ResolveEntry(agentDir string) ([]string, error) {
	versionFile := filepath.Join(agentDir, "install", "current-version")
	if data, err := os.ReadFile(versionFile); err == nil {
		version := strings.TrimSpace(string(data))
		if version != "" && version != "." && version != ".." {
			packageDir := filepath.Join(agentDir, "install", "releases", version,
				"node_modules", "@earendil-works", "pi-coding-agent")
			if entry, err := readPackageEntry(packageDir); err == nil {
				return []string{"node", entry}, nil
			}
		}
	}
	if binary, err := exec.LookPath("pi"); err == nil {
		return []string{binary}, nil
	}
	return nil, fmt.Errorf("pi: no entry point under %s and no pi on PATH", agentDir)
}

// readPackageEntry reads the “bin.pi“ path out of a package.json.
func readPackageEntry(packageDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(packageDir, "package.json"))
	if err != nil {
		return "", err
	}
	var metadata struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", err
	}
	// ``bin`` is either a string (single-command package) or a map.
	var asString string
	if err := json.Unmarshal(metadata.Bin, &asString); err == nil && asString != "" {
		return filepath.Join(packageDir, asString), nil
	}
	var asMap map[string]string
	if err := json.Unmarshal(metadata.Bin, &asMap); err == nil {
		if relative, ok := asMap["pi"]; ok && relative != "" {
			return filepath.Join(packageDir, relative), nil
		}
	}
	return "", errors.New("package.json declares no pi bin")
}

// Pid reports the child's process id, for diagnostics.
func (s *Session) Pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}
