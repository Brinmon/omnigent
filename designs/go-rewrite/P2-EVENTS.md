# Go rewrite — P2: live events

P2 makes the session live: a client can post a message, follow the stream, and
watch the turn's status edges arrive in order. This is the layer the web app's
reducer consumes, so its shape matters more than its size.

## What P2 added

| Surface | Where | Notes |
|---|---|---|
| `GET /v1/sessions/{id}/stream` | `internal/server/stream_handler.go` | snapshot + live tail, SSE |
| `POST /v1/sessions/{id}/events` | `internal/server/events.go` | accepts input, drives a turn |
| `WS /v1/sessions/updates` | `internal/server/updates_ws.go` | sidebar deltas, replaces polling |
| fan-out | `internal/stream/hub.go` | ordering, slow-consumer isolation, wire format |

## Two routes that are not in `openapi.json`

`POST .../events` and `WS /v1/sessions/updates` are both absent from the
document, for different and instructive reasons:

- **`POST /events`** is marked `include_in_schema=False` in the Python server.
  Its response is a small acknowledgement, not a domain model, so it was never
  published. The generated router therefore has no handler, and P2 registers it
  directly.
- **`WS /sessions/updates`** is a WebSocket. OpenAPI cannot describe a handshake,
  so no document could carry it.

Both are still part of the contract the web client depends on, which is why
`updates_ws_test.go` and `stream_test.go` pin their protocols rather than
trusting the schema. A route missing from `openapi.json` is not a route that can
drift freely.

## The echo turn

P2 has no runner and no harness, so `POST /events` runs an **echo turn**: it
walks the same status edges a real turn does and publishes the same frame kinds,
with the reply explicitly marked:

```json
{"type": "response.completed", "text": "...", "echo": true}
```

The `echo` flag exists so a transcript from this build cannot be mistaken for a
model response. That matters because the frames are otherwise identical — which
is the point: a client that renders an echo turn correctly will render a real
one, since the sequence is the same.

The sequence, asserted by `TestPostMessageProducesTurnEdges`:

```
session.status  running
response.completed  (echo)
session.status  idle
```

## Sequence numbers are null here, and that is correct

Every frame carries `sequence_number: null`. That looked like a bug while
reading the Python output until the schema explained it:

> `sequence_number`: assigned by the SSE serializer at emit time. `None` on
> session-scoped events emitted directly by the runtime (the session stream does
> not number events).

Only events flowing through the per-response serializer are numbered. Numbering
session-scoped events here would emit a field the client's de-duplication has to
then ignore.

## Slow-consumer isolation

The hub never blocks a publisher. A subscriber whose buffer is full is dropped
and its channel closed; the HTTP handler sees the close and ends the response,
which makes the client reconnect and resynchronise from the snapshot.

That is the right trade: a client that has stopped reading has already fallen
behind, and blocking would delay every other viewer of the same session.
`TestSlowSubscriberIsDroppedNotBlocked` covers it, and deliberately sizes the
healthy subscriber's buffer above the burst — a smaller buffer would test
"buffer smaller than the burst" rather than the isolation property.

## Headers that are load-bearing

```go
"Content-Type":      "text/event-stream; charset=utf-8"
"Cache-Control":     "no-cache"
"X-Accel-Buffering": "no"
"X-Omnigent-Stream-Epoch": "<pid>-<start-nanos>"
```

`X-Accel-Buffering: no` turns off nginx's response buffering. Without it, events
sit in the proxy until its buffer fills and the UI appears frozen — the classic
"SSE works locally but not behind a proxy" failure.

`X-Omnigent-Stream-Epoch` identifies one process's stream generation, so a
client that reconnects after a restart knows its cached snapshot may be stale.

## The updates WebSocket protocol

```
client -> server   {"type":"watch","session_ids":[...]}
server -> client   {"type":"snapshot","items":[...]}   after each watch
                   {"type":"changed","items":[...]}    watched rows changed
                   {"type":"removed","ids":[...]}      watched rows vanished
                   {"type":"heartbeat"}                nothing to say
```

Timings are copied from the Python server so a client's idle handling and a
proxy's timeout behave identically:

| Constant | Value | Source |
|---|---|---|
| rescan interval | 4 s | `_SESSION_UPDATES_RESCAN_INTERVAL_S` |
| heartbeat | 30 s | `_SESSION_UPDATES_HEARTBEAT_INTERVAL_S` |
| max watched | 500 | `_SESSION_UPDATES_MAX_WATCHED` |

Unknown client frames are ignored rather than rejected, so a newer client can add
message types without breaking this server.

Freshness is **pull-based**: each tick re-reads the watched ids and diffs against
the last sent snapshot. Keeping the diff in one place avoids requiring every
write path to publish.

## Verifying

```bash
cd go && go test ./internal/stream/ ./internal/server/ -v
```

| Test | What it pins |
|---|---|
| `TestEncodeMatchesPythonWireFormat` | `event:`/`data:` framing, trailing blank line, nulls |
| `TestPublishDeliversInOrder` | per-subscriber ordering |
| `TestSlowSubscriberIsDroppedNotBlocked` | publisher never blocks |
| `TestDroppedSubscriberChannelCloses` | handler learns to end the response |
| `TestStreamStartsWithPresence` | snapshot-first ordering |
| `TestPostMessageProducesTurnEdges` | running → completed → idle (B4) |
| `TestUpdatesWatchReturnsSnapshot` | sidebar snapshot shape (B5) |
| `TestUpdatesIgnoresUnknownFrames` | forward compatibility |
| `TestStreamSendsSSEHeaders` | the proxy headers above |

## What P2 does not do

- **No runner, no harness.** The echo turn is not a model call; P3 adds the
  `pi` RPC implementation and replaces the echo with a real turn.
- **No persistence.** The echo reply is published but not written to
  `conversation_items`, because there is no `POST /v1/items` path yet. A reload
  therefore loses it; P3's turn writes rows.
- **No presence registry.** `session.presence` reports an empty viewer list,
  which is what the Python server reports when the caller is the only viewer.
- **Browser B4/B5 not yet run.** The Go-side protocol is proven by the tests
  above; the Playwright runs that compare rendered DOM against the Python
  baseline come after P3 gives the UI a real agent to talk to.

## Next: P3

P3 is the V1 critical gate: runner binary, tunnel, `agent/pi` RPC, EventSink →
SSE, and the tool bridge. That is where `session.status` transitions stop being
synthesised and start coming from a real `pi --mode rpc` child process.
