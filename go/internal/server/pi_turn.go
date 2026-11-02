package server

// Driving a real Pi turn.
//
// This replaces P2's echo turn: the input goes to a ``pi --mode rpc`` child, its
// events are translated onto the session stream, and the turn ends when Pi says
// ``agent_settled`` rather than at ``agent_end`` -- an auto-retry emits more than
// one ``agent_end``, so stopping there would truncate a retried turn.
//
// P3 still has no runner process: the server spawns Pi directly. The runner
// split exists so a session can outlive one server and be driven from a host the
// server cannot reach; until that is needed, spawning in-process keeps the
// pipeline (stream -> turn -> Pi -> stream) observable in one place.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/pi"
	"github.com/Brinmon/omnigent/go/internal/stream"
)

// piConfig is the resolved way to launch Pi, or why it cannot be launched.
type piConfig struct {
	entry    []string
	model    string
	agentDir string
	// reason is set when Pi is unavailable, and is what the client is told.
	reason string
}

// resolvePiConfig finds Pi without failing the server: a build with no Pi
// installed still serves the read API and reports why a turn cannot run.
func resolvePiConfig() piConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		return piConfig{reason: "no home directory to locate the Pi agent dir"}
	}
	agentDir := filepath.Join(home, ".pi", "agent")
	entry, err := pi.ResolveEntry(agentDir)
	if err != nil {
		return piConfig{reason: err.Error()}
	}
	// An explicit model wins: which id a provider actually answers with is a
	// deployment fact this build cannot discover, and picking one from the
	// registry means picking one the provider may not serve.
	model := os.Getenv("OMNIGENT_PI_MODEL")
	if model == "" {
		model = defaultPiModel(agentDir)
	}
	return piConfig{
		entry:    entry,
		agentDir: agentDir,
		model:    model,
	}
}

// defaultPiModel picks a provider-qualified model id from Pi's own registry.
//
// The id must be qualified: an unqualified one is ambiguous across providers and
// Pi refuses to start a turn. A caller can override it per turn.
func defaultPiModel(agentDir string) string {
	raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
	if err != nil {
		return ""
	}
	model, err := firstUsableModel(raw)
	if err != nil {
		return ""
	}
	return model
}

// piEnv builds the child environment.
//
// The environment is not merged with the parent: Pi's behaviour depends on which
// variables are present, and inheriting them would make a turn depend on the
// developer's shell. The whitelist below is the minimum Windows needs to start a
// process at all plus what Pi needs to resolve credentials.
func (s *Server) piEnv(agentDir string) map[string]string {
	keep := []string{
		"PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
		"TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA",
		"LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
		"PROGRAMW6432", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		"USERNAME",
	}
	env := make(map[string]string, len(keep)+4)
	for _, key := range keep {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	env["PI_CODING_AGENT_DIR"] = agentDir
	env["NO_PROXY"] = "127.0.0.1,localhost"
	env["PYTHONUTF8"] = "1"

	// Provider credentials come from the same place Pi's own registry reads
	// them, so a developer who can run ``pi`` can run a turn here too.
	if baseURL, apiKey, ok := s.piProviderCredentials(); ok {
		env["OPENAI_BASE_URL"] = baseURL
		env["OPENAI_API_KEY"] = apiKey
	}
	return env
}

// piTurn runs one turn against Pi and publishes its events.
type piTurn struct {
	server    *Server
	sessionID string
	hub       *stream.Hub

	mu      sync.Mutex
	abort   context.CancelFunc
	running bool
}

// runPiTurn starts a turn and publishes as it progresses.
//
// The caller gets an error only for a failure to start; everything after that
// arrives as events, because the client is already rendering.
func (s *Server) runPiTurn(
	ctx context.Context,
	sessionID string,
	sessionIDRaw db.Uuid16,
	hub *stream.Hub,
	prompt string,
) error {
	config := s.pi
	if config.reason != "" {
		return errors.New(config.reason)
	}
	if config.model == "" {
		return errors.New("no model available in Pi's registry")
	}

	turnCtx, cancel := context.WithCancel(ctx)

	workdir, err := os.MkdirTemp("", "omnigent-turn-")
	if err != nil {
		cancel()
		return fmt.Errorf("create turn workspace: %w", err)
	}
	// Pi resolves the model list from PI_CODING_AGENT_DIR; an empty isolated dir
	// makes every model unknown, so the registry is copied in.
	agentDir := filepath.Join(workdir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		cancel()
		return fmt.Errorf("create agent dir: %w", err)
	}
	if source, err := os.ReadFile(filepath.Join(config.agentDir, "models.json")); err == nil {
		_ = os.WriteFile(filepath.Join(agentDir, "models.json"), source, 0o644)
	}

	// The tool bridge is what makes Omnigent's tools visible to Pi and its native
	// tool calls gated. Without it Pi runs entirely inside its own process and
	// nothing here can observe or stop a tool call.
	bridge, extensionPath, cleanupBridge, err := s.startToolBridge(turnCtx, sessionID, workdir)
	if err != nil {
		cancel()
		return fmt.Errorf("start tool bridge: %w", err)
	}
	defer cleanupBridge()
	var extraArgs []string
	if extensionPath != "" {
		extraArgs = append(extraArgs, "--extension", extensionPath)
	}
	_ = bridge

	slog.Debug("pi turn starting",
		"session", sessionID,
		"entry", config.entry,
		"model", config.model,
		"workdir", workdir)
	session, err := pi.Launch(turnCtx, pi.LaunchOptions{
		Entry: config.entry,
		Model: config.model,
		// A non-empty system prompt makes the launcher pass an empty
		// --append-system-prompt, which is what stops Pi from also loading its
		// own APPEND_SYSTEM.md. Without it the model receives a second,
		// unrelated instruction set and answers into it instead of the turn.
		SystemPrompt:  "You are an expert coding assistant operating inside Omnigent.",
		DisableSkills: true,
		ExtraArgs:     extraArgs,
		Cwd:           workdir,
		Env:           s.piEnv(agentDir),
	})
	if err != nil {
		cancel()
		slog.Warn("pi launch failed", "session", sessionID, "error", err)
		return err
	}
	slog.Debug("pi launched", "session", sessionID, "pid", session.Pid())

	s.registerPiSession(sessionID, session)
	defer s.unregisterPiSession(sessionID)

	s.publishStatus(ctx, sessionID, hub, "running")

	// Run the turn to completion here: the caller's goroutine owns the registry
	// slot, and returning early would release the cancel that keeps Pi alive.
	defer cancel()
	defer session.Close()
	defer os.RemoveAll(workdir)
	recorder := newTurnRecorder()
	recorder.addUserMessage(prompt)
	s.consumePiEvents(turnCtx, sessionID, sessionIDRaw, hub, session, prompt, recorder)
	return nil
}

// consumePiEvents translates Pi's stream onto the session stream.
func (s *Server) consumePiEvents(
	ctx context.Context,
	sessionID string,
	sessionIDRaw db.Uuid16,
	hub *stream.Hub,
	session *pi.Session,
	prompt string,
	recorder *turnRecorder,
) {
	slog.Debug("pi prompt sending", "bytes", len(prompt))
	if err := session.Prompt(ctx, prompt); err != nil {
		s.publishPiFailure(ctx, sessionID, hub, "harness", "could not send the prompt: "+err.Error())
		return
	}

	var reply strings.Builder
	for {
		select {
		case <-ctx.Done():
			// The turn was cancelled from outside (client gone or interrupt).
			return
		case event, open := <-session.Events():
			if !open {
				// Pi exited without settling: report it rather than leaving the
				// client on "running" forever.
				s.publishPiFailure(ctx, sessionID, hub, "harness",
					"the harness exited before the turn finished: "+lastLines(session.Stderr(), 3))
				return
			}
			switch event.Type {
			case "message_update":
				if delta := event.TextDelta(); delta != "" {
					reply.WriteString(delta)
					hub.Publish(stream.Event{
						Type: "response.output_text.delta",
						Payload: map[string]any{
							"type":            "response.output_text.delta",
							"conversation_id": sessionID,
							"sequence_number": nil,
							"delta":           delta,
						},
					})
				}

			case "message_end":
				message := event.Message()
				if message["role"] != "assistant" {
					continue
				}
				// message_end carries the authoritative text; the delta sum can
				// miss a segment if Pi restarts a message after a retry.
				if text := piAssistantText(message); text != "" {
					reply.Reset()
					reply.WriteString(text)
				}
				if usage, ok := message["usage"].(map[string]any); ok {
					hub.Publish(stream.Event{
						Type: "session.usage",
						Payload: map[string]any{
							"type":            "session.usage",
							"conversation_id": sessionID,
							"sequence_number": nil,
							"usage":           usage,
						},
					})
				}

			case "auto_retry_start":
				// Surfacing the retry lets the UI show why a turn is slow
				// instead of appearing stalled.
				hub.Publish(stream.Event{
					Type: "response.retry",
					Payload: map[string]any{
						"type":            "response.retry",
						"conversation_id": sessionID,
						"sequence_number": nil,
						"attempt":         event.Raw["attempt"],
						"max_attempts":    event.Raw["maxAttempts"],
						"reason":          event.Raw["errorMessage"],
					},
				})

			case "agent_settled":
				aborted, _ := event.Raw["aborted"].(bool)
				// Persist before announcing the end: a client that receives
				// idle and reloads immediately must find the rows already there.
				recorder.addAssistantMessage(reply.String(), s.pi.model)
				if err := recorder.flush(ctx, s.db, s.workspace, sessionIDRaw); err != nil {
					slog.Warn("could not persist the turn", "session", sessionID, "error", err)
				}
				s.publishTurnEnd(ctx, sessionID, hub, reply.String(), aborted)
				return
			}
		}
	}
}

// publishTurnEnd emits the turn's terminal frames.
func (s *Server) publishTurnEnd(
	ctx context.Context,
	sessionID string,
	hub *stream.Hub,
	text string,
	aborted bool,
) {
	if aborted {
		hub.Publish(stream.Event{
			Type: "response.cancelled",
			Payload: map[string]any{
				"type":            "response.cancelled",
				"conversation_id": sessionID,
				"sequence_number": nil,
			},
		})
	} else {
		hub.Publish(stream.Event{
			Type: "response.completed",
			Payload: map[string]any{
				"type":            "response.completed",
				"conversation_id": sessionID,
				"sequence_number": nil,
				"text":            text,
			},
		})
	}
	s.publishStatus(ctx, sessionID, hub, "idle")
}

// publishPiFailure reports a harness failure the client can render.
func (s *Server) publishPiFailure(
	ctx context.Context,
	sessionID string,
	hub *stream.Hub,
	source, message string,
) {
	slog.Warn("pi turn failed", "session", sessionID, "source", source, "error", message)
	hub.Publish(stream.Event{
		Type: "response.failed",
		Payload: map[string]any{
			"type":            "response.failed",
			"conversation_id": sessionID,
			"sequence_number": nil,
			"error": map[string]any{
				"source":  source,
				"code":    "harness_error",
				"message": message,
			},
		},
	})
	s.publishStatus(ctx, sessionID, hub, "idle")
}

// piAssistantText concatenates the text parts of an assistant message.
func piAssistantText(message map[string]any) string {
	content, ok := message["content"].([]any)
	if !ok {
		if text, ok := message["content"].(string); ok {
			return text
		}
		return ""
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

// lastLines returns the final n non-empty lines of a diagnostic blob.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) <= n {
		return strings.TrimSpace(text)
	}
	return strings.Join(lines[len(lines)-n:], " | ")
}

// piTurnTimeout bounds one turn so a wedged child cannot hold the session in
// "running" forever.
const piTurnTimeout = 30 * time.Minute

// firstUsableModel picks a provider-qualified model id out of Pi's registry.
//
// Provider qualification is required: an unqualified id is ambiguous across
// providers and Pi refuses to start the turn.
func firstUsableModel(raw []byte) (string, error) {
	var registry struct {
		Providers map[string]struct {
			APIKey string `json:"apiKey"`
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return "", err
	}
	// Prefer a provider that carries its own credentials: a registry entry
	// without an apiKey resolves to a provider this build cannot authenticate
	// against, and Pi would start and then fail the turn with an empty reply.
	providers := make([]string, 0, len(registry.Providers))
	for provider := range registry.Providers {
		providers = append(providers, provider)
	}
	sortStrings(providers)

	pick := func(requireKey bool) (string, bool) {
		for _, provider := range providers {
			entry := registry.Providers[provider]
			if len(entry.Models) == 0 {
				continue
			}
			if requireKey && entry.APIKey == "" {
				continue
			}
			// Prefer an id that names a concrete version over a bare alias:
			// an alias like "deepseek-flash" is a provider-side pointer that may
			// resolve to a model whose output Pi discards, while a versioned id
			// is the one the developer actually exercises by hand.
			models := make([]string, 0, len(entry.Models))
			for _, model := range entry.Models {
				models = append(models, model.ID)
			}
			sortStrings(models)
			for _, id := range models {
				if strings.ContainsAny(id, "0123456789") && strings.Contains(id, ".") {
					return provider + "/" + id, true
				}
			}
			return provider + "/" + models[0], true
		}
		return "", false
	}

	if model, ok := pick(true); ok {
		return model, nil
	}
	if model, ok := pick(false); ok {
		return model, nil
	}
	return "", errors.New("models.json defines no models")
}

// piProviderCredentials reads the first provider's endpoint and key from Pi's
// registry, so a developer who can run “pi“ can run a turn here too.
func (s *Server) piProviderCredentials() (string, string, bool) {
	raw, err := os.ReadFile(filepath.Join(s.pi.agentDir, "models.json"))
	if err != nil {
		return "", "", false
	}
	var registry struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return "", "", false
	}
	// The provider matching the chosen model is the one whose credentials
	// matter; the model id carries it as a prefix.
	provider := s.pi.model
	if idx := strings.Index(provider, "/"); idx > 0 {
		provider = provider[:idx]
	}
	if entry, ok := registry.Providers[provider]; ok && entry.BaseURL != "" && entry.APIKey != "" {
		return entry.BaseURL, entry.APIKey, true
	}
	for _, entry := range registry.Providers {
		if entry.BaseURL != "" && entry.APIKey != "" {
			return entry.BaseURL, entry.APIKey, true
		}
	}
	return "", "", false
}

// sortStrings is an insertion sort: the slices here are small, and this keeps
// the package free of an extra import for one call.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// registerPiSession records the live child so interrupt can ask it to abort.
//
// Killing the process instead would skip agent_settled, so the turn would never
// publish its terminal frame and the client would stay on "running".
func (s *Server) registerPiSession(sessionID string, session *pi.Session) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	if s.piSessions == nil {
		s.piSessions = make(map[string]*pi.Session)
	}
	s.piSessions[sessionID] = session
}

// unregisterPiSession drops the record once the turn ends.
func (s *Server) unregisterPiSession(sessionID string) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()
	delete(s.piSessions, sessionID)
}

// abortPiSession asks the running child to stop its turn.
//
// Returns false when no child is registered, which means the turn has not
// reached the launch point yet or has already finished.
func (s *Server) abortPiSession(ctx context.Context, sessionID string) bool {
	s.turnsMu.Lock()
	session := s.piSessions[sessionID]
	s.turnsMu.Unlock()
	if session == nil {
		return false
	}
	// Best effort: a failing abort write still lets the turn end on its own,
	// and the cancel path remains as the outer bound.
	_ = session.Abort(ctx)
	return true
}
