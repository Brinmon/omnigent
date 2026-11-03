package server

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/Brinmon/omnigent/go/internal/api"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// agentRow is the column set both agent queries return, so one renderer serves
// the built-in and the owned listing.
type agentRow struct {
	ID             db.Uuid16
	CreatedAt      int64
	UpdatedAt      *int64
	Name           string
	BundleLocation string
	Version        int64
	Kind           int64
	Description    []byte
}

// defaultAgentPageLimit matches the client's request and the Python default.
const defaultAgentPageLimit = 50

// maxAgentPageLimit bounds a caller-supplied page size.
const maxAgentPageLimit = 1000

// userAgentPageLimit is the cap the Python server applies to scope=user pages.
const userAgentPageLimit = 50

// ListBuiltinAgentsV1AgentsGet lists installed agents.
//
// The new-session picker reads this before it can create anything: with no agents
// it has nothing to bind a session to, so a 501 here makes the create path
// unreachable no matter how correct the create route itself is.
func (s *Server) ListBuiltinAgentsV1AgentsGet(w http.ResponseWriter, r *http.Request,
	params api.ListBuiltinAgentsV1AgentsGetParams) {
	limit := int64(defaultAgentPageLimit)
	if params.Limit != nil && *params.Limit > 0 {
		limit = int64(*params.Limit)
	}
	if limit > maxAgentPageLimit {
		limit = maxAgentPageLimit
	}

	owned := params.Scope != nil && *params.Scope == "user"
	if owned && limit > userAgentPageLimit {
		limit = userAgentPageLimit
	}

	var rows []agentRow
	if owned {
		// scope=user is "the agents this caller installed", which is kind=user
		// AND created_by=caller. Filtering on kind alone would list another
		// user's agents in a shared workspace.
		page, err := s.queries.ListAgentsOwnedBy(r.Context(), store.ListAgentsOwnedByParams{
			WorkspaceID: s.workspace,
			Kind:        db.AgentKind["user"],
			CreatedBy:   &s.userID,
			PageLimit:   limit,
		})
		if err != nil {
			slog.Error("list owned agents", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"could not list the agents")
			return
		}
		for _, row := range page {
			rows = append(rows, agentRow{
				ID: row.ID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
				Name: row.Name, BundleLocation: row.BundleLocation,
				Version: row.Version, Kind: row.Kind, Description: row.Description,
			})
		}
	} else {
		page, err := s.queries.ListAgents(r.Context(), store.ListAgentsParams{
			WorkspaceID: s.workspace,
			Kind:        db.AgentKind["server"],
			PageLimit:   limit,
		})
		if err != nil {
			slog.Error("list agents", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"could not list the agents")
			return
		}
		for _, row := range page {
			rows = append(rows, agentRow{
				ID: row.ID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
				Name: row.Name, BundleLocation: row.BundleLocation,
				Version: row.Version, Kind: row.Kind, Description: row.Description,
			})
		}
	}

	agents := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		agents = append(agents, s.renderAgent(r, row))
	}

	body := map[string]any{
		"object":                 "list",
		"data":                   agents,
		"first_id":               nil,
		"last_id":                nil,
		"has_more":               false,
		"suppressed_agent_names": []string{},
	}
	if len(agents) > 0 {
		body["first_id"] = agents[0]["id"]
		body["last_id"] = agents[len(agents)-1]["id"]
	}
	writeJSON(w, http.StatusOK, body)
}

// renderAgent builds one agent entry.
//
// The collection fields are present-and-empty rather than absent: the client
// reads them with .map, and a missing key throws instead of rendering nothing.
func (s *Server) renderAgent(r *http.Request, row agentRow) map[string]any {
	agent := map[string]any{
		"id":                    row.ID.String(),
		"object":                "agent",
		"name":                  row.Name,
		"version":               row.Version,
		"description":           nil,
		"created_at":            row.CreatedAt,
		"updated_at":            nullInt64(row.UpdatedAt),
		"harness":               nil,
		"mcp_servers":           []any{},
		"mcp_servers_editable":  false,
		"mcp_registry_editable": false,
		"policies":              []any{},
		"skills":                []any{},
		"terminals":             []any{},
		// builtin is derived from the id, not from kind: a built-in is one whose
		// deterministic name-derived id matches (Python: builtin_agent_id). A
		// server-kind row with any other id is a registered template, which the
		// client lets a newer upload supersede.
		"builtin": isBuiltinAgent(row.ID.String(), row.Name),
	}

	if len(row.Description) > 0 {
		if text, err := db.DecodeCompressedText(row.Description); err == nil {
			agent["description"] = text
		} else {
			slog.Warn("agent description unreadable", "agent", row.ID.String(), "error", err)
		}
	}
	// harness and skills come from the bundle rather than columns, so an
	// unreadable blob leaves them empty instead of failing the whole list.
	if spec, ok := s.readAgentBundle(r.Context(), row.BundleLocation); ok {
		if spec.Harness != "" {
			agent["harness"] = spec.Harness
		}
		agent["skills"] = spec.Skills
	}
	return agent
}

// isBuiltinAgent reports whether an id is the deterministic id derived from the
// name, which is how the Python server identifies a built-in.
//
// The id is sha256("builtin:<name>") truncated to 32 hex characters. Computing it
// rather than storing a flag keeps the two builds agreeing without a migration.
func isBuiltinAgent(id, name string) bool {
	digest := sha256.Sum256([]byte("builtin:" + name))
	return id == hex.EncodeToString(digest[:])[:32]
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
