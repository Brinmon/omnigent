package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Brinmon/omnigent/go/internal/bundle"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// createdSession is what a bundled create produces.
type createdSession struct {
	SessionID string
	AgentID   string
	AgentName string
}

// errNoArtifactStore reports a bundled create on a server started without blob
// storage. It is a server misconfiguration, not a client error: the client sent
// a valid request the operator's setup cannot serve.
var errNoArtifactStore = errors.New("artifact store is not configured")

// createSessionFromBundle stores the bundle and writes the four rows a session
// needs, in one transaction.
//
// The blob is written before the transaction and left in place if the
// transaction fails: an orphaned blob is harmless and a later create reuses the
// same key, whereas a committed row pointing at a missing blob breaks every read
// of that session.
func (s *Server) createSessionFromBundle(
	ctx context.Context, spec bundle.Spec, data []byte, title string,
) (createdSession, error) {
	if s.artifacts == nil {
		return createdSession{}, errNoArtifactStore
	}

	agentUUID, err := newUuid16()
	if err != nil {
		return createdSession{}, err
	}
	sessionUUID, err := newUuid16()
	if err != nil {
		return createdSession{}, err
	}
	agentID, sessionID := agentUUID.String(), sessionUUID.String()

	// The bundle's location is the agent id plus the content hash, matching the
	// Python layout so either build can read the other's agents.
	digest := sha256.Sum256(data)
	location := agentID + "/" + hex.EncodeToString(digest[:])
	if err := s.artifacts.Put(location, data); err != nil {
		return createdSession{}, fmt.Errorf("store agent bundle: %w", err)
	}

	now := time.Now().Unix()
	// The Python create leaves agents.description NULL even when the YAML has a
	// description -- it is not part of what the create records, so writing it
	// would make the two builds store different rows for the same upload.
	var descriptionValue []byte

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return createdSession{}, fmt.Errorf("begin create transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queries := s.queries.WithTx(tx)
	if err := insertAgent(ctx, queries, insertAgentParams{
		ID: agentUUID, CreatedAt: now, Name: spec.Name, Location: location,
		Description: descriptionValue, Kind: db.AgentKind["user"], CreatedBy: "local",
	}); err != nil {
		return createdSession{}, err
	}
	if err := insertSession(ctx, queries, insertSessionParams{
		ID: sessionUUID, CreatedAt: now, Title: title, AgentID: agentUUID,
	}); err != nil {
		return createdSession{}, err
	}
	if err := grantOwnership(ctx, queries, sessionUUID); err != nil {
		return createdSession{}, err
	}
	if err := createSessionMetadata(ctx, queries, sessionUUID); err != nil {
		return createdSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return createdSession{}, fmt.Errorf("commit create transaction: %w", err)
	}

	slog.Info("created session", "session", sessionID, "agent", agentID,
		"name", spec.Name, "harness", spec.Harness)
	return createdSession{SessionID: sessionID, AgentID: agentID, AgentName: spec.Name}, nil
}

// setSessionWorkspace records the directory a session runs in.
func (s *Server) setSessionWorkspace(ctx context.Context, q *store.Queries,
	sessionID db.Uuid16, workspace string) error {
	err := q.SetSessionWorkspace(ctx, store.SetSessionWorkspaceParams{
		Workspace:   &workspace,
		WorkspaceID: s.workspace,
		ID:          sessionID,
	})
	if err != nil {
		return fmt.Errorf("record session workspace: %w", err)
	}
	return nil
}

// insertLabels records the labels a create carried, plus the client surface.
//
// The client's create token lives here and is how it matches the session it just
// requested against the list it re-fetches; without it the optimistic temp id is
// never replaced by the real one.
func (s *Server) insertLabels(ctx context.Context, q *store.Queries, sessionID db.Uuid16,
	labels map[string]string, clientSurface string) error {
	merged := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		merged[key] = value
	}
	if _, present := merged["omnigent.client_surface"]; !present {
		merged["omnigent.client_surface"] = clientSurface
	}

	now := time.Now().Unix()
	for key, value := range merged {
		err := q.UpsertLabel(ctx, store.UpsertLabelParams{
			WorkspaceID:    s.workspace,
			ConversationID: sessionID[:],
			Key:            key,
			Value:          value,
			UpdatedAt:      now,
		})
		if err != nil {
			return fmt.Errorf("record label %q: %w", key, err)
		}
	}
	return nil
}

// createSessionForAgent creates a session bound to an existing agent.
//
// It writes the same three companion rows as the bundled path (session,
// ownership grant, metadata) and skips only the agent and blob, which already
// exist. Duplicating the rest would let the two creates diverge in a way only
// one of them is tested for.
func (s *Server) createSessionForAgent(
	ctx context.Context, agentID db.Uuid16, agentName, title, clientSurface string,
	labels map[string]string,
) (db.Uuid16, error) {
	sessionUUID, err := newUuid16()
	if err != nil {
		return db.Uuid16{}, err
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return db.Uuid16{}, fmt.Errorf("begin create transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	queries := s.queries.WithTx(tx)
	if err := insertSession(ctx, queries, insertSessionParams{
		ID: sessionUUID, CreatedAt: now, Title: title, AgentID: agentID,
	}); err != nil {
		return db.Uuid16{}, err
	}
	if err := grantOwnership(ctx, queries, sessionUUID); err != nil {
		return db.Uuid16{}, err
	}
	if err := createSessionMetadata(ctx, queries, sessionUUID); err != nil {
		return db.Uuid16{}, err
	}
	if err := s.insertLabels(ctx, queries, sessionUUID, labels, clientSurface); err != nil {
		return db.Uuid16{}, err
	}
	// The workspace the user picked travels only in the composer-context label, so
	// it is recovered here and stored on the metadata row the read path serves.
	if workspace := workingDirectoryFromLabels(labels); workspace != "" {
		if err := s.setSessionWorkspace(ctx, queries, sessionUUID, workspace); err != nil {
			return db.Uuid16{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return db.Uuid16{}, fmt.Errorf("commit create transaction: %w", err)
	}

	slog.Info("created session for agent", "session", sessionUUID.String(),
		"agent", agentName)
	return sessionUUID, nil
}

type insertAgentParams struct {
	ID          db.Uuid16
	CreatedAt   int64
	Name        string
	Location    string
	Description []byte
	Kind        int64
	CreatedBy   string
}

func insertAgent(ctx context.Context, q *store.Queries, p insertAgentParams) error {
	err := q.InsertAgent(ctx, store.InsertAgentParams{
		WorkspaceID:    0,
		ID:             p.ID,
		CreatedAt:      p.CreatedAt,
		Name:           p.Name,
		BundleLocation: p.Location,
		Version:        1,
		Description:    p.Description,
		UpdatedAt:      &p.CreatedAt,
		Kind:           p.Kind,
		CreatedBy:      &p.CreatedBy,
	})
	if err != nil {
		return fmt.Errorf("insert agent: %w", err)
	}
	return nil
}

type insertSessionParams struct {
	ID        db.Uuid16
	CreatedAt int64
	Title     string
	AgentID   db.Uuid16
}

func insertSession(ctx context.Context, q *store.Queries, p insertSessionParams) error {
	// root_conversation_id points at the session itself: this row is the root of
	// its own tree, and a fork later points its root at this id.
	zeroPosition := int64(0)
	root := p.ID
	err := q.CreateSession(ctx, store.CreateSessionParams{
		WorkspaceID: 0,
		ID:          p.ID,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.CreatedAt,
		Title:       p.Title,
		// A top-level create has no parent; the zero uuid is stored as NULL by
		// Uuid16.Value.
		ParentConversationID: db.Uuid16{},
		RootConversationID:   root,
		NextPosition:         &zeroPosition,
		Archived:             false,
		AgentID:              p.AgentID,
		SessionOverrides:     nil,
	})
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// grantOwnership records the creator as owner.
//
// This is not optional bookkeeping: ListSessions joins on it, so a create that
// skipped this row would make its own session invisible to the client that just
// created it.
func grantOwnership(ctx context.Context, q *store.Queries, sessionID db.Uuid16) error {
	err := q.GrantSessionPermission(ctx, store.GrantSessionPermissionParams{
		UserID:         "local",
		ConversationID: sessionID[:],
		Level:          levelOwner,
		WorkspaceID:    0,
	})
	if err != nil {
		return fmt.Errorf("grant session ownership: %w", err)
	}
	return nil
}

// createSessionMetadata writes the companion row the read path expects.
func createSessionMetadata(ctx context.Context, q *store.Queries, sessionID db.Uuid16) error {
	err := q.CreateSessionMetadata(ctx, store.CreateSessionMetadataParams{
		WorkspaceID: 0,
		ID:          sessionID,
		Kind:        db.ConversationKind["default"],
	})
	if err != nil {
		return fmt.Errorf("create session metadata: %w", err)
	}
	return nil
}
