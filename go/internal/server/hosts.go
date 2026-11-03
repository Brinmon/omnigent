package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// localHostName is what the picker shows for the machine running this server.
const localHostName = "This machine"

// localHostID derives a stable host id for this machine.
//
// It must survive a restart: the client remembers which host the user picked and
// re-selects it by id, so a random id would silently reset that choice on every
// boot. Hashing the machine name rather than the hostname alone keeps two
// services on one machine distinct while staying deterministic.
func localHostID() []byte {
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = "localhost"
	}
	digest := sha256.Sum256([]byte("local-host:" + identity))
	return digest[:16]
}

// RegisterLocalHost records this machine as an execution target.
//
// Without it the client has nothing to select: the new-session composer stays
// disabled with "No host selected" and no session can be started from the landing
// page. The Python server registers a host when a runner connects over a
// websocket; this build runs the harness in-process, so the host is the server
// itself and is registered at startup.
func (s *Server) RegisterLocalHost(ctx context.Context) error {
	now := time.Now().Unix()

	// configured_harnesses tells the picker which harness families this host can
	// run. pi is the only one V1 supports, and its availability is a real fact
	// from startup rather than an assumption: a machine with no Pi install must
	// report it unusable so the client can say why.
	harnesses := map[string]any{"pi": s.pi.available()}
	harnessJSON, err := json.Marshal(harnesses)
	if err != nil {
		return err
	}

	err = s.queries.UpsertLocalHost(ctx, store.UpsertLocalHostParams{
		WorkspaceID: s.workspace,
		HostID:      localHostID(),
		UserID:      s.userID,
		Name:        localHostName,
		// The server is running right now and will spawn the harness itself, so
		// this host is online by construction.
		Status:              db.HostStatus["online"],
		CreatedAt:           &now,
		UpdatedAt:           &now,
		ConfiguredHarnesses: harnessJSON,
	})
	if err != nil {
		return err
	}
	slog.Info("registered local host", "host", hex.EncodeToString(localHostID()),
		"pi", s.pi.available())
	return nil
}

// ListHostsV1HostsGet reports this machine as an execution target.
func (s *Server) ListHostsV1HostsGet(w http.ResponseWriter, r *http.Request) {
	rows, err := s.queries.ListHosts(r.Context(), store.ListHostsParams{
		WorkspaceID: s.workspace,
		UserID:      s.userID,
	})
	if err != nil {
		slog.Error("list hosts", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not list the hosts")
		return
	}

	// Always at least one entry: the server is running, so it is an execution
	// target even if the row has not been written yet. Returning an empty list
	// disables the composer, which is the state this endpoint exists to fix.
	hosts := make([]map[string]any, 0, len(rows)+1)
	for _, row := range rows {
		hosts = append(hosts, renderHost(row))
	}
	if len(hosts) == 0 {
		hosts = append(hosts, map[string]any{
			"host_id":              hex.EncodeToString(localHostID()),
			"name":                 localHostName,
			"owner":                s.userID,
			"status":               "online",
			"sandbox_provider":     nil,
			"configured_harnesses": map[string]any{"pi": s.pi.available()},
			"gateway_inference":    nil,
			"interactive_shells":   nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

// renderHost converts a stored row to the shape the client reads.
func renderHost(row store.ListHostsRow) map[string]any {
	status := "offline"
	if row.Status == db.HostStatus["online"] {
		status = "online"
	}
	harnesses := map[string]any{}
	if len(row.ConfiguredHarnesses) > 0 {
		if err := json.Unmarshal(row.ConfiguredHarnesses, &harnesses); err != nil {
			// A corrupt readiness map must not hide the host: the client treats a
			// missing entry as unknown rather than unusable.
			slog.Warn("host configured_harnesses unreadable", "error", err)
			harnesses = map[string]any{}
		}
	}
	var sandbox any
	if row.SandboxProvider != nil {
		sandbox = *row.SandboxProvider
	}
	return map[string]any{
		"host_id":              hex.EncodeToString(row.HostID),
		"name":                 row.Name,
		"owner":                row.UserID,
		"status":               status,
		"sandbox_provider":     sandbox,
		"configured_harnesses": harnesses,
		"gateway_inference":    nil,
		"interactive_shells":   nil,
	}
}
