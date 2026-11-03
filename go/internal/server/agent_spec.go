package server

import (
	"context"
	"log/slog"

	"github.com/Brinmon/omnigent/go/internal/bundle"
)

// agentSpec is what a session's bound agent declares about how to run.
type agentSpec struct {
	Harness string
	Model   string
	// Skills is the skill list the client renders in the agent's details.
	Skills []any
}

// readAgentSpec re-reads an agent's bundle to recover the harness and model.
//
// These are not columns on the agents table -- the Python server derives them by
// loading the bundle -- so a session read has to do the same or the client loses
// the harness name it renders the credential for. A failure is logged and
// reported as "unknown" rather than failing the read: a session whose blob is
// missing is still worth showing.
func (s *Server) readAgentBundle(ctx context.Context, location string) (agentSpec, bool) {
	if s.artifacts == nil || location == "" {
		return agentSpec{}, false
	}
	data, err := s.artifacts.Get(location)
	if err != nil {
		slog.Warn("agent bundle unreadable; harness and model will be absent",
			"location", location, "error", err)
		return agentSpec{}, false
	}
	spec, err := bundle.Parse(data)
	if err != nil {
		slog.Warn("agent bundle unparseable; harness and model will be absent",
			"location", location, "error", err)
		return agentSpec{}, false
	}
	skills := make([]any, 0, len(spec.Skills))
	for _, skill := range spec.Skills {
		skills = append(skills, map[string]any{
			"name":         skill.Name,
			"description":  skill.Description,
			"display_name": nil,
		})
	}
	return agentSpec{Harness: spec.Harness, Model: spec.Model, Skills: skills}, true
}

// sessionLabels returns the labels a session reports.
//
// The Python create stamps the client surface that opened the session. It is
// recorded as a label rather than a column, so a build that omits it makes the
// client's own telemetry view lose the fact that a session exists.
func sessionLabels(clientSurface string) map[string]string {
	if clientSurface == "" {
		clientSurface = "cli"
	}
	return map[string]string{"omnigent.client_surface": clientSurface}
}

// clientSurfaceOf reads the label value a create should record.
//
// The local build has no browser fingerprinting, so the header is the only
// signal and "cli" is the fallback the Python server uses when it has none.
func clientSurfaceOf(headerValue string) string {
	if headerValue == "" {
		return "cli"
	}
	return headerValue
}

// defaultContextWindow is the fallback the Python server uses when no registry
// knows the model (omnigent/llms/context_window.py: _DEFAULT_CONTEXT_WINDOW).
//
// This build has no litellm registry, so it always falls through to the same
// default. Reporting nothing instead would remove the client's fullness bar for
// every model, including the ones the baseline shows it for.
const defaultContextWindow int64 = 128_000

// contextWindowFor reports the model's context window.
func contextWindowFor(model string) *int64 {
	if model == "" {
		return nil
	}
	window := defaultContextWindow
	return &window
}
