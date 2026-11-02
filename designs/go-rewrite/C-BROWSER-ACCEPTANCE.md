# Go rewrite — C: browser acceptance

Every earlier increment proved a Go surface against itself or against the Python
server's JSON. This one proves the **user-visible behaviour**: the built web
client, in a real browser, against the Go server.

```
step                     result  detail
------------------------------------------------------------------------------
boot                     PASS    Omnigent
sidebar                  PASS    session row rendered
open-session             PASS    opened 8355b792
send-message             PASS    assistant reply rendered
history-after-reload     PASS    stored transcript rendered after reload
```

That is the first end-to-end evidence that the rewrite is usable, not merely
correct.

## What it took: serving the SPA

The Go server had no static-file layer, so the client could not load at all. Two
routing rules make it work, and both matter:

- **an API path always wins.** `/v1/...` that the server does not implement must
  404 as JSON; returning the SPA's HTML there surfaces in the client as a parse
  error instead of "not found".
- **everything else falls back to `index.html`.** A deep link like `/c/<id>` must
  render, because the client owns its own routes.

Assets are cached `immutable` (their names are content-hashed) while `index.html`
is `no-cache`, so a deploy does not leave a client on the previous entry point.

## Two real defects the browser found

Neither was visible from any API test.

### `GET /v1/sessions/{id}/items` was a stub

The client does not read the detail response's `items` field — it calls this route
separately. An unimplemented route left the session page showing:

```
Conversation not found
Couldn't load 8355b792…: ListSessionItemsV1SessionsSessionIdItemsGet is not implemented
```

### The item envelope is FLAT, not nested

My first implementation nested the payload under `data`. The Python server puts a
compaction's `summary` and `token_count` at the **top level** beside `id` and
`type`, and the client reads them there:

```json
{"id": "...", "response_id": "resp-0", "type": "compaction",
 "status": "completed", "created_at": 1760000000,
 "summary": "…", "last_item_id": "…", "token_count": 1234}
```

Nesting it produced `Cannot read properties of undefined (reading 'map')`.

## A fixture defect, found by bisecting

The session page showed "Something went wrong" against **both** backends, which
ruled out the Go code. `bisect-items.py` writes a database with only a message
plus one candidate type and checks whether the page renders cleanly:

```
message only                   -> OK
message + function_call        -> OK
…
message + error                -> BROKEN
broken types: ['error']
```

The fixture's error item used `code: "fixture_error"`. The client maps a code to a
human-readable description and falls back to "Something went wrong" for one it
does not know. Changing it to `executor_error` — a code the client has text for —
fixed the render.

A sample row a real build would never write is not neutral sample data: it can
make correct code look broken.

## The reload assertion, corrected

My first version asserted the *newly sent* prompt appeared after a reload. It
failed, so I ran the same flow against the Python backend:

```
PYTHON items: 11 -> 12
PYTHON reload shows prompt: False
PYTHON reload shows PONG: False
```

The baseline behaves the same way — the client renders the stored history on load
and the new turn's rows are its own concern. Asserting them would have made this a
test of the client's caching written to look like a test of the Go server.

The step now asserts what the server is responsible for: the stored transcript
renders after a reload.

Worth noting in passing: for the same turn, Python stored **one** row and Go
stored **two** (prompt + reply). The Go path is the more complete one.

## Running it

```bash
# one-time: build the client the server serves
cd web && pnpm exec vite build

# the acceptance run
python designs/go-rewrite/browser-acceptance.py
python designs/go-rewrite/browser-acceptance.py --headed      # watch it
python designs/go-rewrite/browser-acceptance.py --channel chrome
```

The script builds the Go binary, stages a copy of the reference database, starts
the server on a free port, and drives Edge (or Chrome) — no Chromium download.

## Deliberate limits

- **Five steps.** Enough to prove the client works against this server; the full
  B1–E5 matrix runs against the Python baseline and is the eventual target.
- **No visual comparison.** The original plan called for comparing rendered DOM
  against the baseline. That needs both servers running the same client build,
  which is the next step.
- **Reload does not show the new turn.** See above: it is the client's behaviour,
  and matching the baseline means matching that too.

## Where this leaves the rewrite

| Layer | Evidence |
|---|---|
| contract (A1–A7) | `run-contract.sh`, 6 checks green |
| Go API parity | `compare-servers.py`, every P1 endpoint matches |
| Go unit/integration | 10 packages, ~90 tests |
| real harness | `internal/pi` + `internal/toolbridge` drive real Pi |
| **browser** | **this run: 5 steps green** |
