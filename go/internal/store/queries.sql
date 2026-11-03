-- P1 queries: the read surface the sidebar and session page need.
--
-- Names describe the caller's intent rather than the SQL shape, matching the
-- convention the Python stores use (see the repo's database-query-names note).
-- Every query here is exercised against the Python-written reference database,
-- so a schema drift fails a test rather than a request.

-- name: GetSession :one
SELECT id, created_at, updated_at, title, parent_conversation_id,
       root_conversation_id, next_position, workspace_id, archived, agent_id,
       session_overrides
FROM conversations
WHERE workspace_id = ? AND id = ?;

-- name: ListSessions :many
-- The sidebar list. Mirrors the Python store's default listing:
--   * kind "default" means top-level sessions, expressed as parent IS NULL;
--   * sub-agent conversations are reached through their parent, not the list;
--   * archived sessions are excluded unless include_archived is set;
--   * the caller must hold a grant, which is what the ACL join enforces.
-- Newest first, keyset-friendly: pagination walks (created_at, id).
SELECT c.id, c.created_at, c.updated_at, c.title, c.parent_conversation_id,
       c.root_conversation_id, c.next_position, c.workspace_id, c.archived,
       c.agent_id, c.session_overrides
FROM conversations AS c
WHERE c.workspace_id = ?
  AND c.parent_conversation_id IS NULL
  AND (sqlc.arg('include_archived') OR c.archived = 0)
  AND (sqlc.narg('after_created_at') IS NULL
       OR c.created_at < sqlc.narg('after_created_at'))
  AND (sqlc.narg('before_created_at') IS NULL
       OR c.created_at > sqlc.narg('before_created_at'))
  AND EXISTS (
      SELECT 1 FROM session_permissions AS p
      WHERE p.workspace_id = c.workspace_id
        AND p.conversation_id = c.id
        AND p.user_id = sqlc.arg('user_id')
  )
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg('page_limit');

-- name: CountSessions :one
-- Same filters as ListSessions so the sidebar's count matches its rows.
SELECT count(*) FROM conversations AS c
WHERE c.workspace_id = ?
  AND c.parent_conversation_id IS NULL
  AND (sqlc.arg('include_archived') OR c.archived = 0)
  AND EXISTS (
      SELECT 1 FROM session_permissions AS p
      WHERE p.workspace_id = c.workspace_id
        AND p.conversation_id = c.id
        AND p.user_id = sqlc.arg('user_id')
  );

-- name: CreateSession :exec
INSERT INTO conversations (
    workspace_id, id, created_at, updated_at, title, parent_conversation_id,
    root_conversation_id, next_position, archived, agent_id, session_overrides
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSessionTitle :exec
UPDATE conversations SET title = ?, updated_at = ? WHERE workspace_id = ? AND id = ?;

-- name: SetSessionArchived :exec
UPDATE conversations SET archived = ?, updated_at = ? WHERE workspace_id = ? AND id = ?;

-- name: DeleteSession :exec
DELETE FROM conversations WHERE workspace_id = ? AND id = ?;

-- name: GetSessionMetadata :one
SELECT id, kind, runner_id, host_id, sub_agent_name, external_session_id,
       workspace, git_branch, runner_last_seen, live_status,
       pending_elicitation_count, project_id, task_summary
FROM omnigent_conversation_metadata
WHERE workspace_id = ? AND id = ?;

-- name: UpsertSessionMetadata :exec
INSERT INTO omnigent_conversation_metadata (
    workspace_id, id, kind, runner_id, host_id, sub_agent_name,
    external_session_id, workspace, git_branch, runner_last_seen, live_status,
    pending_elicitation_count, project_id, task_summary
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (workspace_id, id) DO UPDATE SET
    kind = excluded.kind,
    runner_id = excluded.runner_id,
    host_id = excluded.host_id,
    sub_agent_name = excluded.sub_agent_name,
    external_session_id = excluded.external_session_id,
    workspace = excluded.workspace,
    git_branch = excluded.git_branch,
    runner_last_seen = excluded.runner_last_seen,
    live_status = excluded.live_status,
    pending_elicitation_count = excluded.pending_elicitation_count,
    project_id = excluded.project_id,
    task_summary = excluded.task_summary;

-- name: ListLabels :many
SELECT key, value FROM conversation_labels
WHERE workspace_id = ? AND conversation_id = ?
ORDER BY key;

-- name: UpsertLabel :exec
INSERT INTO conversation_labels (workspace_id, conversation_id, key, value, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (workspace_id, conversation_id, key) DO UPDATE SET
    value = excluded.value, updated_at = excluded.updated_at;

-- name: DeleteLabel :exec
DELETE FROM conversation_labels
WHERE workspace_id = ? AND conversation_id = ? AND key = ?;

-- name: CountItems :one
SELECT count(*) FROM conversation_items WHERE workspace_id = ? AND conversation_id = ?;

-- name: ListItems :many
-- Ascending by position: the transcript renders in order, and the client pages
-- forward from a position rather than by time.
SELECT id, conversation_id, response_id, created_at, position, type, status,
       created_by
FROM conversation_items
WHERE workspace_id = ? AND conversation_id = ?
  AND (sqlc.narg('after_position') IS NULL OR position > sqlc.narg('after_position'))
ORDER BY position ASC
LIMIT sqlc.arg('page_limit');

-- name: GetItemData :one
SELECT data, search_text FROM conversation_items
WHERE workspace_id = ? AND conversation_id = ? AND id = ?;

-- name: GetAgent :one
SELECT id, created_at, updated_at, name, bundle_location, version, kind,
       description, created_by
FROM agents
WHERE workspace_id = ? AND id = ?;

-- name: ListAgents :many
SELECT id, created_at, updated_at, name, bundle_location, version, kind,
       description, created_by
FROM agents
WHERE workspace_id = ? AND kind = ?
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: GetCommentFingerprint :one
-- Comments count and the newest mutation time, which the sidebar uses as a
-- change fingerprint: an add or edit bumps updated_at, a delete changes the
-- count, so the client invalidates its cached comment list on either.
-- updated_at is stored at microsecond precision (see the API's
-- comments_updated_at), so no conversion happens here.
SELECT count(*) AS comment_count, max(updated_at) AS last_updated_at
FROM comments
WHERE workspace_id = ? AND conversation_id = ?;

-- name: AllocateNextPosition :one
-- Reads and atomically advances the conversation's position counter.
--
-- The Python store keeps a counter rather than running MAX(position) per append,
-- so allocation stays O(1) and collision-free under the conversation lock. A
-- conversation created before the counter existed has NULL here; the caller
-- falls back to a one-time MAX scan.
UPDATE conversations
SET next_position = COALESCE(next_position, 0) + 1
WHERE workspace_id = ? AND id = ?
RETURNING COALESCE(next_position, 1) - 1 AS position;

-- name: AllocateNextPositionRange :one
-- Allocates n consecutive positions in one statement, for a batch append.
--
-- The start is computed in Go from the post-update counter: repeating a numbered
-- parameter in RETURNING makes sqlc emit too few arguments for SQLite's binding.
UPDATE conversations
SET next_position = COALESCE(next_position, 0) + ?
WHERE workspace_id = ? AND id = ?
RETURNING next_position AS position;

-- name: AppendConversationItem :exec
INSERT INTO conversation_items (
    workspace_id, conversation_id, id, response_id, created_at, position,
    data, search_text, created_by, type, status
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: MaxItemPosition :one
-- The fallback for a conversation whose counter is NULL.
SELECT COALESCE(MAX(position), -1) FROM conversation_items
WHERE workspace_id = ? AND conversation_id = ?;

-- name: InsertAgent :exec
INSERT INTO agents (
    workspace_id, id, created_at, name, bundle_location, version, description,
    updated_at, kind, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GrantSessionPermission :exec
INSERT INTO session_permissions (user_id, conversation_id, level, workspace_id)
VALUES (?, ?, ?, ?);

-- name: CreateSessionMetadata :exec
INSERT INTO omnigent_conversation_metadata (workspace_id, id, kind)
VALUES (?, ?, ?);

-- name: ListAgentsOwnedBy :many
-- scope=user lists only the agents this caller installed. Filtering on kind
-- alone would also return another user's agents in a shared workspace.
SELECT id, created_at, updated_at, name, bundle_location, version, kind,
       description, created_by
FROM agents
WHERE workspace_id = sqlc.arg('workspace_id')
  AND kind = sqlc.arg('kind')
  AND created_by = sqlc.arg('created_by')
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');
