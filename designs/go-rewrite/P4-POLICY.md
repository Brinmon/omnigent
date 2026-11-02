# Go rewrite — P4: the policy engine

The tool bridge made tool calls *visible*. The policy engine makes them
*stoppable*: a DENY now prevents the handler from running, which is the
difference between a guardrail and a log line.

## The engine

Rules are CEL predicates over an `event`:

```json
[{"name":"deny-write","tools":["omnigent_write_file"],"expression":"true",
  "action":"DENY","reason":"writes are frozen","enabled":true}]
```

| Decision | Meaning | Enforced how |
|---|---|---|
| `ALLOW` | proceed | the handler runs |
| `ASK` | park until a human answers | blocked-with-a-reason in the bridge |
| `DENY` | refuse | the handler never runs |

Available to an expression:

```
event.type      "request" | "tool_call" | "tool_result" | "response"
event.target    the tool name (empty for a request phase)
event.text      the prompt or tool argument text
event.data      an open-ended bag
```

## Three decisions worth naming

### Precedence, not a vote: `DENY > ASK > ALLOW`

An allow-list that can outvote a deny-list provides no protection. The engine
takes the most restrictive applicable decision, and `TestDenyOutranksAllow` fails
if that ever becomes a vote or a first-match.

`DENY` also outranks `ASK` — a human should not be asked to approve something
another policy already forbids.

### An unevaluable rule fails closed

A CEL expression that errors at runtime yields `DENY`, not `ALLOW`. The reasoning
is the same as precedence: the rule that cannot be judged must not be the one
that lets an action through. `TestEvaluationErrorFailsClosed` covers it.

The same applies at load time — a rule that does not compile is a startup error,
because a policy that silently never fires is worse than one that refuses to
start.

### The gate runs inside the executor, not beside it

```go
execute, err := s.gatedToolHandler(workdir, ctx, sessionID)
```

The policy check and the tool execution live in one handler, so there is no
window between "allowed" and "ran", and no second place that could forget to
check. `gatedToolHandler` wraps `executeBridgedTool` rather than replacing it.

## The `ASK` limitation, stated plainly

Pi's `tool_call` hook can only return allow-or-block; it has no way to park a
call while a human answers. So `ASK` is currently reported as a block whose
reason names the approval requirement.

Making `ASK` a real approval card needs the response flow — the ASK event, the
elicitation round-trip, and the client's card — not this hook. The engine already
produces the decision; only its presentation is pending. `TestPolicyDeny...`
proves the path, and the ASK round-trip is what D1's card test will exercise.

## Verified

```bash
cd go && go test ./internal/policy/ -v          # 18 tests
cd go && go test ./internal/server/ -run TestPolicy -v
```

| Test | Proves |
|---|---|
| `TestDenyOutranksAllow` | a deny-list cannot be outvoted |
| `TestDenyOutranksAsk` | a forbidden action is not offered for approval |
| `TestDenyPreventsExecution` | **a denied call never reaches the handler** |
| `TestAllowedCallExecutes` | the gate does not stop what it has no opinion about |
| `TestEvaluationErrorFailsClosed` | an unevaluable rule denies |
| `TestCompileErrorsAreRejected` | a bad policy fails at startup, not silently |
| `TestToolFilterNeverMatchesRequestPhase` | a tool rule cannot fire on a prompt |
| `TestReservedFieldsAreNotShadowedByData` | `event.data` cannot forge `event.type` |
| `TestPolicyDenyStopsBridgedTool` | real turn, real deny, turn still settles |

The last one is the end-to-end shape: the model was asked to call a denied tool,
the call was stopped, and the turn reached a terminal frame rather than hanging.

## Configuration

`OMNIGENT_POLICY_RULES` holds the JSON array. A malformed value panics at
startup rather than degrading to "no policies" — an operator who set the variable
must not silently lose the guardrail.

A file or database source arrives with the policy store; `policyRules`
(compiled from the `policies` table) has `handler`, `type`, and `scope`
columns waiting for it.

## Deliberate limits

- **No policy store.** Rules come from the environment. The table exists and its
  columns are known; reading it is a query away.
- **No ASK round-trip.** See above.
- **`PHASE_REQUEST` is not wired.** The engine supports the phase and
  `TestPhaseFilterScopesARule` covers it, but nothing evaluates it yet: a request
  phase has no tool call to stop, so enforcement there means gating the turn
  itself.
- **Native-tool verdicts are not persisted.** A blocked native call leaves no
  `function_call` row, because Pi never executed it.

## Next

The ASK round-trip: an `ASK` verdict emits an elicitation, the client renders the
card, and the answer resumes or cancels the call. That is what turns the
documented `ASK` limitation into the feature D1 asserts.