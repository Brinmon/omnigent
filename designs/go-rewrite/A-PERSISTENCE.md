# Go rewrite — A: persisting the transcript

A turn streamed live but was not written, so a reload lost it. This increment
closes that: the prompt and the reply are stored as `conversation_items` rows
shaped exactly as the Python store writes them.

This is what B3 (history hydration) and E3 (reload dedup) assert against, and it
is a prerequisite for the tool bridge: a tool call that is not recorded cannot be
compared after a reload.

## What it does

`turnRecorder` (`internal/server/turn_persist.go`) accumulates a turn's rows and
writes them once, immediately before the terminal frame:

```
user message       position N
assistant message  position N+1
```

Two decisions worth naming:

- **one batch at the end, not per event.** A turn produces exactly two rows, so
  writing per event would put a database round-trip between every delta for no
  benefit.
- **no row for an empty reply.** An empty bubble reads as "the agent said
  nothing", which is worse than an absent reply the `response.failed` event
  already explains.

## Positions come from the counter, not MAX()

```sql
UPDATE conversations SET next_position = COALESCE(next_position, 0) + ?
WHERE workspace_id = ? AND id = ?
RETURNING next_position;
```

The start of the range is derived in Go (`end - count`), which keeps the
statement to **one** occurrence of the parameter. That detail is load-bearing —
see the sqlc finding below.

## Three defects this surfaced

### 1. sqlc emits too few arguments for a repeated numbered parameter

The obvious form is:

```sql
SET next_position = COALESCE(next_position, 0) + sqlc.arg('count')
...
RETURNING next_position - sqlc.arg('count') AS position
```

sqlc generated three arguments (`Count`, `WorkspaceID`, `ID`) while SQLite binds
four, so every write failed at runtime:

```
allocate positions: missing argument with index 4
```

The fix is to reference the parameter once and do the arithmetic in Go. Worth
remembering: sqlc's `sqlc.arg` is positional, not a variable.

### 2. Interrupt killed the process instead of aborting the turn

`interruptTurn` called the context's `cancel()`, which terminated Pi outright.
Pi never emitted `agent_settled`, so the turn never published
`response.cancelled` and the client stayed on "running" — a 120 s test timeout
was the symptom.

The fix registers the live child per session and sends Pi's own `abort` command:

```
agent_settled {"aborted": true}   ->   response.cancelled   ->   idle
```

The interrupt test went from a 120 s timeout to 0.55 s.

### 3. The fixture's position counter disagreed with its rows

`reference.db` had `next_position = 100` while the rows reached `position = 103`,
so an append reused positions that already existed. Ordering and keyset
pagination both depend on the counter being ahead of every stored row.

A pre-existing conversation with a NULL counter falls back to a `MAX(position)`
scan, exactly as the Python store does, so an upgraded database pays once.

## Verified

```bash
cd go && go test ./internal/server/ -run 'TestTurnPersists|TestTurnRows|TestInterruptedTurn' -v
```

| Test | Proves |
|---|---|
| `TestTurnPersistsTranscript` | prompt + reply both readable after the turn |
| `TestTurnRowsShareAResponseID` | the pair is grouped, which is how the client pairs them |
| `TestInterruptedTurnStillPersistsPrompt` | an interrupt does not erase what was asked |

A real run: `items before: 11` → `items after: 13`, with

```
pos: 12  "Say PONG"   role=user
pos: 13  "PONG"       role=assistant, model=openai/deepseek-v4.1-flash
```

## Deliberate limits

- **Only messages are written.** A turn that produces tool calls will need
  `function_call` / `function_call_output` rows; that arrives with the tool
  bridge.
- **No `search_text` indexing beyond the row.** The FTS mirror the Python store
  maintains is separate; search still reads the stored column.
- **The write is best-effort.** A failure is logged, not surfaced as a failed
  turn: the user's reply did arrive, and losing the row is recoverable whereas
  discarding the turn is not.

## Next: B, the tool bridge

The loopback TCP tool server, the generated JS extension, and the
`pi.on("tool_call")` policy hook. That is what turns `PHASE_TOOL_CALL` gating
from a stub into enforcement, and what C2 and the D-group acceptance rows need.
It also needs `function_call` row persistence, which is the natural continuation
of this increment.
