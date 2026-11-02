# Go rewrite — B: the tool bridge

Pi runs its own tools in-process. Without a bridge, Omnigent cannot see a tool
call, let alone stop one — every guardrail the policy engine exists to enforce
would be bypassed by the harness doing exactly what it was asked to do.

The bridge flips that: Omnigent's tools are registered *into* Pi, and Pi's native
tools are gated *through* Omnigent.

## Two directions

```
Omnigent tool          registered into Pi  ->  call comes back over loopback
Pi native tool         pi.on("tool_call") ->  verdict comes back, Pi runs it
```

That asymmetry is the whole design. A bridged tool's **execution** happens in
Omnigent; a native tool's execution stays in Pi and only its **verdict** comes
from Omnigent. Getting this backwards would either double-execute a tool or
never gate the ones that matter.

## The wire

One JSON object per line over loopback TCP:

```
tool call        {"id","token","tool","args","call_id"}   ->  {"id","result"} | {"id","error"}
policy eval      {"id","token","kind":"policy_eval",...}   ->  {"id","verdict"} | {"id"}
```

`kind:"policy_eval"` is evaluate-only. Nothing is executed: Pi runs the tool
itself on allow, so executing it here would run it twice.

## Five decisions worth naming

### The token is per turn and compared in constant time

`secrets.token_urlsafe(32)` equivalent (32 bytes, base64url), compared with
`subtle.ConstantTimeCompare`. A timing side channel on a loopback socket is still
reachable by any local process, and each turn gets its own token so one turn's
extension cannot call another's bridge.

### A policy error fails open

A wedged policy engine must not break Pi's native tools — the extension's own
transport errors behave the same way. A *successful* evaluation that returns
`block` is enforced; a *failed* one allows.

### Bridged tools are skipped in the `tool_call` hook

They are already evaluated when the call reaches the bridge. Evaluating again
would prompt the user twice for one call — the double-dialog bug the Python
implementation documents.

### The extension is written into the turn's workspace

A leftover extension in a shared directory would be loaded by an unrelated Pi
process. The workspace dies with the turn, and cleanup closes the bridge *before*
removing the file so a straggling call gets a connection error rather than a route
to a handler whose workspace is already gone.

### The bridged path re-checks containment

`omnigent_read_file` / `omnigent_write_file` bypass the HTTP resource API
entirely, so they cannot rely on it. Absolute paths are rejected rather than
reinterpreted by `filepath.Join`, `..` is rejected, and the resolved target is
checked against both the workspace and its symlink-resolved form.

## Verified

```bash
cd go && go test ./internal/toolbridge/ -v      # 18 tests
cd go && go test ./internal/toolbridge/ -run TestRealPiCallsBridgedTool -v
```

| Test | Proves |
|---|---|
| `TestGenerateExtensionIsValidJavaScript` | node parses the output; a syntax error would abort Pi at startup |
| `TestExtensionEmbedsPortAndToken` | a quote in the token does not break the JS literal |
| `TestExtensionHookSkipsBridgedTools` | the guard runs *before* evaluation (double-prompt) |
| `TestExtensionSkipsUnnamedTools` | one malformed spec does not poison the extension |
| `TestBridgeExecutesTool` | the call reaches the handler with its args and call id |
| `TestBridgeRejectsWrongToken` | forged frames are refused and still acknowledged |
| `TestPolicyEvalDoesNotExecute` | **a verdict never runs the tool** |
| `TestPolicyEvalFailsOpen` | a policy error allows rather than blocks |
| `TestRealPiCallsBridgedTool` | **real Pi, real extension, real call** |

The end-to-end run:

```
bridge received 1 call(s): [omnigent_probe] args=[{}]
--- PASS: TestRealPiCallsBridgedTool (6.12s)
```

That is C2's shape: the model chose a tool, Pi routed it through the generated
extension, and the Go handler ran.

## A note on what the e2e test does *not* prove

It asserts the call arrives. It logs, but does not require, the model echoing the
result back — a model may call a tool and summarize instead of quoting it, and
making that an assertion would produce a flaky test of the model rather than of
the bridge.

## Deliberate limits

- **No policy engine.** The native-tool handler logs and allows. The *path* is
  live, which is what makes D1's `PHASE_TOOL_CALL` gate a matter of filling in a
  verdict rather than building plumbing.
- **Two tools.** `omnigent_read_file` and `omnigent_write_file` read/write the
  turn workspace. The bridge does not care how many there are; the set grows with
  the resource API.
- **Tool calls are not persisted.** A `function_call` / `function_call_output`
  pair does not yet reach `conversation_items`, so a reload shows the messages
  but not the calls between them. That is the next increment, and it is the same
  write path the transcript already uses.

## Next

Persist tool calls, then P4's policy engine on top of this bridge: CEL over
`PHASE_REQUEST` / `PHASE_TOOL_CALL`, with the ASK round-trip that D1 exercises.
