package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Brinmon/omnigent/go/internal/bundle"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// seedAgent is the agent this build ships with.
//
// It exists so the new-session picker has something to bind. The Python server
// seeds seventeen packaged agents; V1 needs one that can actually run, and adding
// more is a matter of extending this list rather than changing the mechanism.
type seedAgent struct {
	Name        string
	Description string
	Harness     string
	YAML        string
}

var seedAgents = []seedAgent{{
	Name:        "pi",
	Description: "A plain chat agent backed by the Pi harness.",
	Harness:     "pi",
	YAML: `name: pi
description: A plain chat agent backed by the Pi harness.
executor:
  harness: pi
prompt: |
  You are a helpful assistant. Answer concisely and say what you did.
`,
}}

// SeedBuiltinAgents installs the packaged agents into the database.
//
// Without this GET /v1/agents returns nothing, and the client's new-session
// picker has no agent to select -- the second half of what keeps the composer
// disabled after the host is available.
//
// The id is derived from the name rather than random, matching the Python
// server: a random id would change on every boot and dangle the agent_id a
// session already points at.
func (s *Server) SeedBuiltinAgents(ctx context.Context) error {
	if s.artifacts == nil {
		// No blob store means the bundle cannot be read back, so seeding would
		// create a row whose harness resolves to null. The server still serves
		// its read API; it simply cannot offer an agent.
		slog.Warn("no artifact store; packaged agents are not installed")
		return nil
	}

	now := time.Now().Unix()
	for _, agent := range seedAgents {
		bundleBytes, err := gzipBytes(agent.YAML)
		if err != nil {
			return fmt.Errorf("pack %s: %w", agent.Name, err)
		}

		// Validate before storing: a packaged bundle that does not parse would
		// otherwise install an agent that fails on its first turn.
		spec, err := bundle.Parse(bundleBytes)
		if err != nil {
			return fmt.Errorf("packaged agent %s is invalid: %w", agent.Name, err)
		}

		id := builtinAgentID(agent.Name)
		digest := sha256.Sum256(bundleBytes)
		location := id + "/" + hex.EncodeToString(digest[:])
		if err := s.artifacts.Put(location, bundleBytes); err != nil {
			return fmt.Errorf("store %s bundle: %w", agent.Name, err)
		}

		err = s.queries.UpsertBuiltinAgent(ctx, store.UpsertBuiltinAgentParams{
			WorkspaceID:    s.workspace,
			ID:             mustUuid16(id),
			CreatedAt:      now,
			Name:           agent.Name,
			BundleLocation: location,
			Version:        1,
			Kind:           db.AgentKind["server"],
			CreatedBy:      nil,
		})
		if err != nil {
			return fmt.Errorf("install agent %s: %w", agent.Name, err)
		}
		slog.Info("installed packaged agent", "name", spec.Name, "id", id,
			"harness", spec.Harness)
	}
	return nil
}

// builtinAgentID derives the deterministic id the Python server uses for a
// built-in agent (omnigent/db/utils.py: builtin_agent_id).
func builtinAgentID(name string) string {
	digest := sha256.Sum256([]byte("builtin:" + name))
	return hex.EncodeToString(digest[:])[:32]
}

// gzipBytes packs a single YAML into the gzipped tar the bundle reader accepts.
func gzipBytes(yamlText string) ([]byte, error) {
	return writeTarball(map[string]string{"agent.yaml": yamlText}, gzip.DefaultCompression)
}

// mustUuid16 parses a derived id, panicking on a programming error rather than
// writing a zero id that no lookup would match.
func mustUuid16(hexID string) db.Uuid16 {
	parsed, err := db.ParseUuid16(hexID)
	if err != nil {
		panic(fmt.Sprintf("invalid builtin uuid %q: %v", hexID, err))
	}
	return parsed
}

// writeTarball packs files into a gzipped tar, the shape the bundle reader and the
// client's upload both use.
func writeTarball(files map[string]string, level int) ([]byte, error) {
	var buffer bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buffer, level)
	if err != nil {
		return nil, err
	}
	writer := tar.NewWriter(gz)
	// Sort the names so the archive bytes are deterministic; the agent id is
	// derived from the content hash, so an unstable order would change it.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, err
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
