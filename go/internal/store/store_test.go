package store_test

// The P1 store layer must work against a database the Python build wrote.
// These tests use the same reference fixture as the A5/A6 contract checks, so a
// schema or encoding drift surfaces here as well as at the contract gate.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

const referenceDB = "../../testdata/reference.db"

// openReference opens the Python-written fixture read-only.
func openReference(t *testing.T) (*sql.DB, *store.Queries) {
	t.Helper()
	if _, err := os.Stat(referenceDB); err != nil {
		t.Fatalf("missing %s; run `python designs/go-rewrite/make-reference-db.py`: %v",
			referenceDB, err)
	}
	abs, err := filepath.Abs(referenceDB)
	if err != nil {
		t.Fatalf("resolve fixture: %v", err)
	}
	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+"?mode=ro")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping fixture: %v", err)
	}
	return conn, store.New(conn)
}

// TestListSessionsReadsPythonRows is the P1 smoke test: the sidebar's list
// query returns the conversations the Python build wrote, with ids intact.
func TestListSessionsReadsPythonRows(t *testing.T) {
	_, queries := openReference(t)
	ctx := context.Background()

	rows, err := queries.ListSessions(ctx, store.ListSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: false,
		PageLimit:       100,
	})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	// The sidebar lists top-level, unarchived sessions the caller owns. The
	// fixture holds one such row; the sub-agent and archived rows are reached
	// another way, which the next test covers.
	if len(rows) != 1 {
		t.Fatalf("got %d conversations, want 1", len(rows))
	}

	// Ids must decode to the 32-char hex form the API serves, not raw bytes.
	for _, row := range rows {
		if got := len(row.ID.String()); got != 32 {
			t.Errorf("id %q is %d hex chars, want 32", row.ID.String(), got)
		}
		if row.Title == "" {
			t.Errorf("conversation %s has an empty title", row.ID)
		}
	}

	total, err := queries.CountSessions(ctx, store.CountSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: false,
	})
	if err != nil {
		t.Fatalf("CountSessions: %v", err)
	}
	if total != 1 {
		t.Errorf("count is %d, want 1", total)
	}
}

// TestNullableUuidColumns covers the shape that breaks naive scanning: a root
// conversation has a NULL parent, and the driver hands back NULL for it. If the
// generated type cannot hold that, the detail read fails for every session.
func TestNullableUuidColumns(t *testing.T) {
	_, queries := openReference(t)
	ctx := context.Background()

	rows, err := queries.ListSessions(ctx, store.ListSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: false,
		PageLimit:       100,
	})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no top-level sessions in the fixture")
	}

	// The listed row is top-level, so its stored parent is NULL.
	listed := rows[0]
	if !listed.ParentConversationID.IsZero() {
		t.Errorf("listed session %s has a parent; it should be top-level", listed.ID)
	}
	if listed.RootConversationID != listed.ID {
		t.Errorf("root %s != id %s for a top-level session", listed.RootConversationID, listed.ID)
	}

	// A sub-agent row has a parent, and scanning it must give a real id.
	child, err := queries.GetSession(ctx, store.GetSessionParams{
		WorkspaceID: 0,
		ID:          db.MustParseUuid16(childSessionID(t)),
	})
	if err != nil {
		t.Fatalf("GetSession(child): %v", err)
	}
	if child.ParentConversationID.IsZero() {
		t.Error("child session reports no parent")
	}
	if child.RootConversationID == child.ID {
		t.Error("child session reports itself as its own root")
	}
}

// TestArchivedFlagReadsAsBool checks the BOOLEAN column shape and the default
// exclusion: SQLite stores 0/1, the fixture's forked session is archived, and
// the sidebar list hides it unless include_archived is set.
func TestArchivedFlagReadsAsBool(t *testing.T) {
	_, queries := openReference(t)
	ctx := context.Background()

	visible, err := queries.ListSessions(ctx, store.ListSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: false,
		PageLimit:       100,
	})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, row := range visible {
		if row.Archived {
			t.Errorf("session %s is archived but appears in the default list", row.ID)
		}
	}

	withArchived, err := queries.ListSessions(ctx, store.ListSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: true,
		PageLimit:       100,
	})
	if err != nil {
		t.Fatalf("ListSessions(include_archived): %v", err)
	}
	archived := 0
	for _, row := range withArchived {
		if row.Archived {
			archived++
		}
	}
	if archived != 1 {
		t.Errorf("%d archived conversations with include_archived, want 1", archived)
	}
	if len(withArchived) <= len(visible) {
		t.Errorf("include_archived returned %d rows, same as the default %d",
			len(withArchived), len(visible))
	}
}

// TestListItemsReadsEveryType walks the transcript query and asserts every
// stored item type decodes to a name, which is what the API returns.
func TestListItemsReadsEveryType(t *testing.T) {
	_, queries := openReference(t)
	ctx := context.Background()

	rows, err := queries.ListSessions(ctx, store.ListSessionsParams{
		WorkspaceID:     0,
		UserID:          "local",
		IncludeArchived: false,
		PageLimit:       100,
	})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	// The fixture puts most items on the conversation with the most rows.
	var target db.Uuid16
	var most int64
	for _, row := range rows {
		count, err := queries.CountItems(ctx, store.CountItemsParams{
			WorkspaceID:    0,
			ConversationID: row.ID,
		})
		if err != nil {
			t.Fatalf("CountItems(%s): %v", row.ID, err)
		}
		if count > most {
			most, target = count, row.ID
		}
	}
	if most == 0 {
		t.Fatal("no items in the fixture")
	}

	items, err := queries.ListItems(ctx, store.ListItemsParams{
		WorkspaceID:    0,
		ConversationID: target,
		PageLimit:      100,
	})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if int64(len(items)) != most {
		t.Errorf("listed %d items, count said %d", len(items), most)
	}

	seen := map[string]bool{}
	for _, item := range items {
		name, err := db.DecodeName(db.ItemTypeName, "conversation_items.type", item.Type)
		if err != nil {
			t.Errorf("item %s has unknown type %d: %v", item.ID, item.Type, err)
			continue
		}
		seen[name] = true
		if item.Position < 0 {
			t.Errorf("item %s has a negative position", item.ID)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only saw %d distinct item types; the fixture should cover all", len(seen))
	}

	// Ascending position is what the transcript renderer relies on.
	for i := 1; i < len(items); i++ {
		if items[i].Position < items[i-1].Position {
			t.Fatalf("items are not ordered by position: %d then %d",
				items[i-1].Position, items[i].Position)
		}
	}
}

// TestAgentReadsCompressedDescription is the store-level counterpart of the A6
// compression check: the description column arrives framed and must decode.
func TestAgentReadsCompressedDescription(t *testing.T) {
	_, queries := openReference(t)
	ctx := context.Background()

	agents, err := queries.ListAgents(ctx, store.ListAgentsParams{
		WorkspaceID: 0,
		Kind:        db.AgentKind["server"],
		PageLimit:   10,
	})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("no server agents in the fixture")
	}

	for _, agent := range agents {
		if len(agent.Description) == 0 {
			t.Errorf("agent %s has no description bytes", agent.ID)
			continue
		}
		if agent.Description[0] != 0x00 {
			t.Errorf("agent %s description is not framed: %x", agent.ID, agent.Description[:4])
			continue
		}
		text, err := db.DecodeCompressedText(agent.Description)
		if err != nil {
			t.Errorf("agent %s: decode description: %v", agent.ID, err)
			continue
		}
		if text == "" {
			t.Errorf("agent %s decoded to empty text", agent.ID)
		}
	}
}

// childSessionID returns the seeded sub-agent conversation id, read from the
// fixture manifest rather than hardcoded so regenerating the fixture keeps
// working.
func childSessionID(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/reference_ids.json")
	if err != nil {
		t.Fatalf("read reference_ids.json: %v", err)
	}
	var ids struct {
		Conversations map[string]string `json:"conversations"`
	}
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatalf("parse reference_ids.json: %v", err)
	}
	id, ok := ids.Conversations["child"]
	if !ok {
		t.Fatal("fixture has no 'child' conversation id")
	}
	return id
}

// A second start must not fail: the schema is applied on every boot, so creating
// an existing table is the normal case rather than an error.
func TestEnsureSchemaIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	conn, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	defer func() { _ = conn.Close() }()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := store.EnsureSchema(conn); err != nil {
			t.Fatalf("EnsureSchema attempt %d: %v", attempt, err)
		}
	}

	var tables int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables < 10 {
		t.Fatalf("only %d tables exist; the schema did not apply", tables)
	}
}
