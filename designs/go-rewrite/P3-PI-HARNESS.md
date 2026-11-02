# Go rewrite — P3: the real Pi harness

P3 is the V1 critical gate: `session.status` transitions stop being synthesised
and start coming from a real `pi --mode rpc` child process.

## What P3 added

| Piece | Where | What it does |
|---|---|---|
| Pi RPC client | `internal/pi/session.go` | spawns Pi, speaks JSONL, parses events |
| turn driver | `internal/server/pi_turn.go` | translates Pi events onto the session stream |
| turn lifecycle | `internal/server/events.go` | one turn per session, interrupt cancels it |

The P2 echo turn is gone: `POST /events` now launches Pi.

## The protocol, as measured

`designs/go-rewrite/probe-pi-rpc.py` drives one turn against the developer's
real install and records what crosses the pipe. A healthy turn:

```
response          {"command":"prompt","success":true,"data":{"disposition":"started"}}
agent_start
turn_start
message_start / message_update / message_end     x3  (system, user, assistant)
turn_end
agent_end
agent_settled     {"aborted":false}              <-- the turn boundary
```

### `agent_end` is not the turn boundary

An auto-retry emits **another** `agent_end`. The probe caught a failing turn
with four of them:

```
agent_end x4   turn_end x4   auto_retry_start x3   agent_settled x1
```

A client that stops at `agent_end` truncates a retried turn. `agent_settled`
(Pi ≥ 0.80.4) is the boundary, and it carries `aborted` — which is how a user
interrupt is distinguished from a completed turn.

### Streaming arrives as `text_delta`

Only `message_update` → `assistantMessageEvent.type == "text_delta"` carries
user-visible text. `text_start` and `text_end` bracket it and have no content.
`message_end` repeats the full message, so it is the authoritative text — the
delta sum can miss a segment when Pi restarts a message after a retry.

## Three Windows problems the Python side also has

### `pi` is a `.cmd` shim that cannot be piped

`pi` on Windows resolves to an npm `.cmd` wrapper. Writing to its stdin fails:

```
OSError: [Errno 22] Invalid argument
```

`cmd.exe` does not forward the handle. The managed install also exposes the real
entry, so `ResolveEntry` reads `install/current-version`, then the package's
`package.json` `bin.pi`, and runs `node dist/bundle/cli.js`. The `.cmd` shim is
only a fallback for an install that ships a native binary.

### `PI_CODING_AGENT_DIR` must contain `models.json`

Isolating the agent dir keeps a developer's global Pi packages out (a broken
extension aborts Pi at startup), but an empty dir makes **every model unknown**,
because Pi resolves model ids against that file. The turn copies `models.json`
into the isolated dir: the isolation is about packages, not the model list.

### The model id must be provider-qualified

```
Error: Model "deepseek-v4.1-flash" is ambiguous across providers:
opencode-go/…, opencode/…, qwen-token-plan-cn/…
```

`openai/deepseek-v4.1-flash` is unambiguous. Beyond qualification, *which* id a
provider actually serves is a deployment fact, so `OMNIGENT_PI_MODEL` overrides
the choice and the test suite pins one the provider is known to answer.

## A bug worth recording

While diagnosing an empty reply I added a debug line that did:

```go
fmt.Sprint(message["content"])[:200]   // panics when content is shorter
```

That panic was swallowed by the server's `recoverJSON` middleware, so the
symptom was a turn that silently produced nothing — no log, no error, just an
empty `response.completed`. Two lessons, both now covered:

- the recovery middleware is right to answer JSON, but a panicking handler and a
  handler that returns empty look identical from the client. Panics are logged
  by `_logger.exception`; check the log before concluding "the model returned
  nothing";
- a slice of a dynamic value needs a length guard. Debug code is not exempt.

## Verified

```bash
cd go && go test ./...                      # 8 packages
cd go && go test ./internal/pi/ -v          # real Pi, real provider
```

| Test | Proves |
|---|---|
| `TestResolveEntryFindsManagedInstall` | the `.cmd` shim is bypassed |
| `TestLaunchAndPrompt` | real turn: ack, `text_delta`, `agent_settled` |
| `TestAbortSettlesTurn` | interrupt yields `agent_settled{aborted:true}` |
| `TestPostMessageProducesTurnEdges` | presence → running → delta → usage → completed |

A real end-to-end run through the HTTP API:

```
event: session.presence
event: session.status              {"status":"running"}
event: response.output_text.delta  {"delta":"PONG"}
event: session.usage               {"totalTokens":…}
event: response.completed          {"text":"PONG"}
```

## Deliberate limits of P3

- **No runner process.** The server spawns Pi directly. The runner split exists
  so a session can outlive one server and be driven from a host the server
  cannot reach; until that is needed, an in-process spawn keeps
  stream → turn → Pi → stream observable in one place.
- **No persistence.** A turn's messages are streamed but not written to
  `conversation_items`, so a reload loses the transcript. That is the next
  increment.
- **No tool bridge.** Pi runs its own tools; the loopback TCP tool server and the
  generated JS extension (which let Omnigent observe and gate tool calls) are
  not wired yet. C2 and the D-group acceptance rows depend on it.
- **No compaction.** C7 waits on the same.

## Next

P4 (policy and resources) needs the tool bridge, because `PHASE_TOOL_CALL`
gating only exists once a tool call passes through Omnigent rather than staying
inside Pi. Before that, the transcript needs to persist so acceptance can compare
a reload against a live turn.
