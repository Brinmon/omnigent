package server_test

// GET /v1/info is the client's boot handshake: it reads these flags before
// rendering, and a missing required field throws during the first render. The
// template is a literal (see internal/server/info.go), so it needs its own
// guard against drifting from the document.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// requiredInfoFields are the document's required keys for ServerInfoResponse.
// Kept as a list so the test names what broke.
var requiredInfoFields = []string{
	"accounts_enabled",
	"branding",
	"databricks_features",
	"dictation_available",
	"enabled_connections",
	"features",
	"harness_install_enabled",
	"installable_harnesses",
	"login_url",
	"managed_sandboxes_enabled",
	"needs_setup",
	"public_sharing_enabled",
	"sandbox_provider",
	"sandbox_providers",
	"server_version",
	"sharing_mode",
	"single_user",
	"smart_routing_enabled",
	"smart_routing_sources",
}

// TestInfoHasEveryRequiredField proves the handshake cannot 200 without a field
// the client dereferences.
func TestInfoHasEveryRequiredField(t *testing.T) {
	ts := newTestServer(t)

	status, body := getRaw(t, ts, "/v1/info")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/info: status %d, want 200: %s", status, body)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/info: %v", err)
	}
	for _, key := range requiredInfoFields {
		if _, ok := got[key]; !ok {
			t.Errorf("/v1/info is missing required field %q", key)
		}
	}
}

// TestInfoMatchesPythonShape compares against the captured Python response, so
// a field the local build stops reporting is visible here rather than as a
// blank panel.
func TestInfoMatchesPythonShape(t *testing.T) {
	ts := newTestServer(t)
	g := loadGolden(t)

	var want map[string]json.RawMessage
	if err := json.Unmarshal(g["/v1/info"].Body, &want); err != nil {
		t.Fatalf("parse golden /v1/info: %v", err)
	}
	_, body := getRaw(t, ts, "/v1/info")
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/info: %v", err)
	}

	// Environment-dependent fields are excluded: the Python capture ran against
	// its own harness inventory, which a Go build does not share.
	ignored := map[string]bool{
		"installable_harnesses":  true,
		"session_event_types":    true,
		"features":               true,
		"filesystem_attachments": true,
	}
	for key := range want {
		if ignored[key] {
			continue
		}
		if _, ok := got[key]; !ok {
			t.Errorf("/v1/info no longer reports %q, which Python does", key)
		}
	}
	for key := range got {
		if ignored[key] {
			continue
		}
		if _, ok := want[key]; !ok {
			t.Errorf("/v1/info reports %q, which Python does not", key)
		}
	}
}
