package server_test

// P1 acceptance: the Go server must answer the endpoints the web client needs
// to boot, with the same shapes the Python server returns. These tests read the
// golden responses captured from a live Python server, so "matches" means
// matches the real thing rather than matches my reading of the schema.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Brinmon/omnigent/go/internal/server"
)

const (
	referenceDB = "../../testdata/reference.db"
	goldenPath  = "../../testdata/http_golden.json"
)

// golden holds the responses captured from the Python server.
type golden map[string]struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("missing %s; capture it from a live Python server: %v", goldenPath, err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse %s: %v", goldenPath, err)
	}
	return g
}

// newTestServer builds the Go server over a copy of the reference database.
//
// A copy, not the fixture: P1 does not write, but a future test will, and
// silently mutating a checked-in oracle is the kind of failure that is only
// noticed later.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	src, err := os.ReadFile(referenceDB)
	if err != nil {
		t.Fatalf("missing %s; run `python designs/go-rewrite/make-reference-db.py`: %v",
			referenceDB, err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	if err := os.WriteFile(dbPath, src, 0o644); err != nil {
		t.Fatalf("stage database: %v", err)
	}
	// Discard the WAL side files the fixture may carry.
	_ = os.Remove(dbPath + "-shm")
	_ = os.Remove(dbPath + "-wal")

	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	handler := server.New(conn, "0.21.0.dev0").Router()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// getRaw performs a GET and returns the status and body.
func getRaw(t *testing.T, ts *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, body
}

// TestMeMatchesPython is the smallest P1 check: identity survives the rewrite.
func TestMeMatchesPython(t *testing.T) {
	ts := newTestServer(t)
	g := loadGolden(t)

	status, body := getRaw(t, ts, "/v1/me")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/me: status %d, want 200", status)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/me: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(g["/v1/me"].Body, &want); err != nil {
		t.Fatalf("parse golden /v1/me: %v", err)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("/v1/me %s = %v, want %v", key, got[key], wantValue)
		}
	}
}

// TestInfoCarriesRequiredFields checks the boot handshake: every field the
// document marks required must be present, because the client reads them
// without guarding for absence.
func TestInfoCarriesRequiredFields(t *testing.T) {
	ts := newTestServer(t)

	status, body := getRaw(t, ts, "/v1/info")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/info: status %d, want 200", status)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/info: %v", err)
	}

	// These are the required fields that decide what the client renders. They
	// are asserted by name rather than by comparing the whole body, because the
	// optional fields legitimately differ between a local single-user install
	// and the captured environment.
	required := []string{
		"accounts_enabled", "single_user", "needs_setup", "databricks_features",
		"managed_sandboxes_enabled", "sandbox_providers", "enabled_connections",
		"sharing_mode", "public_sharing_enabled", "server_version",
		"smart_routing_enabled", "features", "harness_install_enabled",
		"installable_harnesses", "dictation_available", "branding",
	}
	for _, key := range required {
		if _, ok := got[key]; !ok {
			t.Errorf("/v1/info is missing required field %q", key)
		}
	}

	// The values that describe this deployment shape.
	if got["single_user"] != true {
		t.Errorf("single_user = %v, want true for a local install", got["single_user"])
	}
	if got["server_version"] != "0.21.0.dev0" {
		t.Errorf("server_version = %v, want the version passed to New", got["server_version"])
	}
}

// TestHarnessesEndpointAnswers covers the capability matrix the client uses to
// decide which UI affordances to show. P1 has no runner, so the list is empty;
// what matters is the envelope shape, which the client dereferences.
func TestHarnessesEndpointAnswers(t *testing.T) {
	ts := newTestServer(t)

	status, body := getRaw(t, ts, "/v1/harnesses")
	if status == http.StatusNotImplemented {
		t.Fatalf("GET /v1/harnesses is a stub; the client needs it to boot")
	}
	if status != http.StatusOK {
		t.Fatalf("GET /v1/harnesses: status %d, want 200", status)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/harnesses: %v", err)
	}
	// The Python server returns {data: [...], setup_steps: [...]}; the client
	// iterates data unconditionally.
	if _, ok := got["data"]; !ok {
		t.Errorf("/v1/harnesses has no data array: %v", got)
	}
}

// TestListSessionsRendersPythonRows is the sidebar check: the conversations the
// Python build wrote come back with their ids, titles, and timestamps intact.
func TestListSessionsRendersPythonRows(t *testing.T) {
	ts := newTestServer(t)
	g := loadGolden(t)

	status, body := getRaw(t, ts, "/v1/sessions")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/sessions: status %d, want 200: %s", status, body)
	}

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			Id        string  `json:"id"`
			Title     *string `json:"title"`
			CreatedAt int64   `json:"created_at"`
			UpdatedAt int64   `json:"updated_at"`
			Archived  *bool   `json:"archived"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse /v1/sessions: %v", err)
	}

	// The golden response came from a different database, so counts differ.
	// What must match is the envelope and the per-item shape.
	var want struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(g["/v1/sessions"].Body, &want); err != nil {
		t.Fatalf("parse golden /v1/sessions: %v", err)
	}
	if got.Object != want.Object {
		t.Errorf("object = %q, want %q", got.Object, want.Object)
	}

	// The sidebar lists top-level, unarchived sessions: the fixture holds one.
	// Sub-agent and archived rows are excluded by the same rules the Python
	// server applies, which is what this assertion pins.
	if len(got.Data) != 1 {
		t.Fatalf("got %d sessions, want 1 top-level unarchived session", len(got.Data))
	}
	for _, item := range got.Data {
		if len(item.Id) != 32 {
			t.Errorf("id %q is not 32 hex chars", item.Id)
		}
		if item.Title == nil {
			t.Errorf("session %s has no title", item.Id)
		}
		if item.CreatedAt == 0 || item.UpdatedAt == 0 {
			t.Errorf("session %s has zero timestamps", item.Id)
		}
		if item.Archived == nil {
			t.Errorf("session %s has no archived flag; the client groups on it", item.Id)
		}
	}
}

// TestGetSessionRejectsMalformedID checks the 404 mapping: the client treats a
// 404 as "gone", so a malformed id must not surface as a 500.
func TestGetSessionRejectsMalformedID(t *testing.T) {
	ts := newTestServer(t)

	for _, id := range []string{"not-a-uuid", "123", "zzzz"} {
		status, body := getRaw(t, ts, "/v1/sessions/"+id)
		if status != http.StatusNotFound {
			t.Errorf("GET /v1/sessions/%s: status %d, want 404 (%s)", id, status, body)
		}
	}
}

// TestGetSessionReturnsOneRow checks the detail read, which the session page
// issues on navigation.
func TestGetSessionReturnsOneRow(t *testing.T) {
	ts := newTestServer(t)

	status, body := getRaw(t, ts, "/v1/sessions")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/sessions: %d", status)
	}
	var list struct {
		Data []struct {
			Id    string  `json:"id"`
			Title *string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(list.Data) == 0 {
		t.Fatal("no sessions to fetch")
	}

	first := list.Data[0]
	status, body = getRaw(t, ts, "/v1/sessions/"+first.Id)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/sessions/%s: status %d, want 200: %s", first.Id, status, body)
	}
	var one struct {
		Id    string  `json:"id"`
		Title *string `json:"title"`
	}
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if one.Id != first.Id {
		t.Errorf("detail id %q != list id %q", one.Id, first.Id)
	}
	if one.Title == nil || first.Title == nil || *one.Title != *first.Title {
		t.Errorf("detail title %v != list title %v", one.Title, first.Title)
	}
}

// TestUnknownSessionIs404 covers the missing-row path, which the client uses to
// clear a stale link.
func TestUnknownSessionIs404(t *testing.T) {
	ts := newTestServer(t)

	status, _ := getRaw(t, ts, "/v1/sessions/0123456789abcdef0123456789abcdef")
	if status != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", status)
	}
}

// TestErrorsAreJSON checks the envelope: the client parses error bodies, so a
// plain-text 404 would surface as a parse error rather than "not found".
func TestErrorsAreJSON(t *testing.T) {
	ts := newTestServer(t)

	status, body := getRaw(t, ts, "/v1/sessions/not-a-uuid")
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error body is not JSON (%q): %v", body, err)
	}
	if envelope.Error.Code == "" {
		t.Errorf("error envelope has no code: %s", body)
	}
}
