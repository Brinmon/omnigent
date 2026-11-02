package policy_test

// The engine's contract is the precedence, not the expression evaluation: CEL
// already has tests. What matters here is that a deny-list cannot be outvoted by
// an allow-list, that a rule with a bad expression fails closed, and that a
// tool-scoped rule never fires for a phase that has no tool.

import (
	"context"
	"strings"
	"testing"

	"github.com/Brinmon/omnigent/go/internal/policy"
)

// mustEngine compiles rules or fails the test.
func mustEngine(t *testing.T, rules []policy.Rule) *policy.Engine {
	t.Helper()
	engine, err := policy.NewEngine(rules)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// TestEmptyEngineAllows covers the default: no policies means no restriction,
// which is what a fresh install and every P3-era turn already had.
func TestEmptyEngineAllows(t *testing.T) {
	engine := mustEngine(t, nil)
	result := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash",
	})
	if result.Action != policy.Allow {
		t.Errorf("action = %v, want ALLOW", result.Action)
	}
	if !engine.Empty() {
		t.Error("engine with no rules does not report itself empty")
	}
	// A nil engine must behave the same as an empty one: callers pass nil for
	// "no policy configured".
	var nilEngine *policy.Engine
	if got := nilEngine.Evaluate(context.Background(), policy.Event{}).Action; got != policy.Allow {
		t.Errorf("nil engine action = %v, want ALLOW", got)
	}
}

// TestDenyOutranksAllow is the precedence that makes a deny-list useful.
//
// With a vote or with first-match, an allow-list can cancel a deny-list; that
// would make a guardrail advisory. Precedence is how the engine stays a
// guardrail.
func TestDenyOutranksAllow(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "allow-everything",
			Expression: "true",
			Action:     policy.Allow,
			Enabled:    true,
		},
		{
			Name:       "deny-rm",
			Tools:      []string{"bash"},
			Expression: `event.target == "bash"`,
			Action:     policy.Deny,
			Reason:     "bash is not permitted",
			Enabled:    true,
		},
	})

	result := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash",
	})
	if result.Action != policy.Deny {
		t.Fatalf("action = %v, want DENY (an allow-list must not outvote a deny-list)", result.Action)
	}
	if result.Policy != "deny-rm" {
		t.Errorf("deciding policy = %q, want deny-rm", result.Policy)
	}
	if result.Reason != "bash is not permitted" {
		t.Errorf("reason = %q", result.Reason)
	}
}

// TestDenyOutranksAsk: a human should not be asked to approve something another
// policy already forbids.
func TestDenyOutranksAsk(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "ask-bash", Expression: "true", Action: policy.Ask, Reason: "please confirm", Enabled: true},
		{Name: "deny-bash", Expression: "true", Action: policy.Deny, Reason: "forbidden", Enabled: true},
	})
	result := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseToolCall, Target: "bash"})
	if result.Action != policy.Deny {
		t.Errorf("action = %v, want DENY", result.Action)
	}
}

// TestAskWhenNothingDenies covers the middle decision, which is what produces the
// approval card the UI renders.
func TestAskWhenNothingDenies(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "allow-read", Tools: []string{"read"}, Expression: "true", Action: policy.Allow, Enabled: true},
		{Name: "ask-write", Tools: []string{"write"}, Expression: "true", Action: policy.Ask, Reason: "confirm the write", Enabled: true},
	})
	result := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseToolCall, Target: "write"})
	if result.Action != policy.Ask {
		t.Fatalf("action = %v, want ASK", result.Action)
	}
	if result.Policy != "ask-write" || result.Reason != "confirm the write" {
		t.Errorf("result = %+v", result)
	}
}

// TestPhaseFilterScopesARule: a request-phase rule must not fire for a tool call.
func TestPhaseFilterScopesARule(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "request-only",
			Phases:     []policy.Phase{policy.PhaseRequest},
			Expression: "true",
			Action:     policy.Deny,
			Enabled:    true,
		},
	})

	if got := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseRequest}).Action; got != policy.Deny {
		t.Errorf("request phase action = %v, want DENY", got)
	}
	if got := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseToolCall, Target: "bash"}).Action; got != policy.Allow {
		t.Errorf("tool_call action = %v, want ALLOW (the rule is request-scoped)", got)
	}
}

// TestToolFilterNeverMatchesRequestPhase: a request phase has no target, so a
// tool-scoped rule cannot apply to it. Treating an empty target as a match would
// make every tool rule fire on every prompt.
func TestToolFilterNeverMatchesRequestPhase(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "deny-bash",
			Tools:      []string{"bash"},
			Expression: "true",
			Action:     policy.Deny,
			Enabled:    true,
		},
	})
	if got := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseRequest}).Action; got != policy.Allow {
		t.Errorf("request phase action = %v, want ALLOW (a tool rule must not fire)", got)
	}
}

// TestDisabledRuleIsSkipped covers the enable flag, which is how an operator turns
// a policy off without deleting it.
func TestDisabledRuleIsSkipped(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "off", Expression: "true", Action: policy.Deny, Enabled: false},
	})
	if !engine.Empty() {
		t.Error("a disabled rule was compiled")
	}
	if got := engine.Evaluate(context.Background(), policy.Event{}).Action; got != policy.Allow {
		t.Errorf("action = %v, want ALLOW", got)
	}
}

// TestEvaluationErrorFailsClosed is the guardrail property: a rule that cannot be
// judged must not be the reason an action proceeds.
func TestEvaluationErrorFailsClosed(t *testing.T) {
	// `event.data` is absent from the map for this event only if the engine
	// flattens it; the expression dereferences a missing numeric field, which CEL
	// reports as an error rather than false.
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "broken-at-runtime",
			Expression: `event.data.missing > 1`,
			Action:     policy.Allow,
			Enabled:    true,
		},
	})
	result := engine.Evaluate(context.Background(), policy.Event{Type: policy.PhaseToolCall, Target: "bash"})
	if result.Action != policy.Deny {
		t.Errorf("action = %v, want DENY (an unevaluable policy must fail closed)", result.Action)
	}
	if !strings.Contains(result.Reason, "could not be evaluated") {
		t.Errorf("reason = %q, want it to explain the evaluation failure", result.Reason)
	}
}

// TestCompileErrorsAreRejected: a policy that silently never fires is worse than
// a startup failure, so a bad expression must not load.
func TestCompileErrorsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		rule policy.Rule
	}{
		{"syntax error", policy.Rule{Name: "bad-syntax", Expression: "event.target ==", Enabled: true}},
		{"non-bool result", policy.Rule{Name: "not-bool", Expression: `event.target`, Enabled: true}},
		{"empty expression", policy.Rule{Name: "empty", Expression: "   ", Enabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := policy.NewEngine([]policy.Rule{tc.rule}); err == nil {
				t.Error("a rule that cannot be compiled was accepted")
			}
		})
	}
}

// TestDefaultActionIsDeny: a rule written without an action must stop the action,
// because the safe default for a guardrail is to refuse.
func TestDefaultActionIsDeny(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "no-action", Expression: "true", Enabled: true},
	})
	if got := engine.Evaluate(context.Background(), policy.Event{}).Action; got != policy.Deny {
		t.Errorf("action = %v, want DENY", got)
	}
}

// TestTextAndDataAreVisibleToExpressions covers the two fields a content policy
// needs: the text under inspection and the open-ended bag.
func TestTextAndDataAreVisibleToExpressions(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "deny-secret",
			Expression: `event.text.contains("SECRET") || event.data.environment == "prod"`,
			Action:     policy.Deny,
			Reason:     "sensitive content",
			Enabled:    true,
		},
	})

	denied := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash", Text: "cat SECRET_KEY",
	})
	if denied.Action != policy.Deny {
		t.Errorf("text match: action = %v, want DENY", denied.Action)
	}

	deniedByData := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash", Data: map[string]any{"environment": "prod"},
	})
	if deniedByData.Action != policy.Deny {
		t.Errorf("data match: action = %v, want DENY", deniedByData.Action)
	}

	allowed := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash", Text: "ls -la",
		Data: map[string]any{"environment": "dev"},
	})
	if allowed.Action != policy.Allow {
		t.Errorf("non-match: action = %v, want ALLOW", allowed.Action)
	}
}

// TestReservedFieldsAreNotShadowedByData: the data bag is nested rather than
// flattened, so a rule reading “event.type“ cannot be redirected by a caller
// that supplies its own "type" field.
func TestReservedFieldsAreNotShadowedByData(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{
			Name:       "deny-tool-call",
			Expression: `event.type == "tool_call"`,
			Action:     policy.Deny,
			Enabled:    true,
		},
	})
	// A caller supplies a conflicting "type" inside data; the phase field is what
	// the rule must see.
	result := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseRequest,
		Data: map[string]any{"type": "tool_call"},
	})
	if result.Action != policy.Allow {
		t.Errorf("action = %v, want ALLOW: event.data must not shadow event.type", result.Action)
	}
}

// TestRulesListIsStableForDiagnostics covers the accessor the server logs at
// startup, which must not depend on map iteration order.
func TestRulesListIsStableForDiagnostics(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "zeta", Expression: "true", Enabled: true},
		{Name: "alpha", Expression: "true", Enabled: true},
		{Name: "mid", Expression: "true", Enabled: true},
	})
	names := engine.Rules()
	want := []string{"alpha", "mid", "zeta"}
	if len(names) != len(want) {
		t.Fatalf("Rules() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Rules() = %v, want %v", names, want)
		}
	}
}
