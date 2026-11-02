package policy_test

// The gate must stop an action, not merely record an objection. These tests
// exercise the engine against a real tool handler so a regression that turns
// enforcement into logging fails loudly.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/Brinmon/omnigent/go/internal/policy"
)

// runGated models the server's gate: evaluate, then execute only on ALLOW.
//
// The shape mirrors internal/server/gatedToolHandler so a change in one is
// visible against the other.
func runGated(
	t *testing.T,
	engine *policy.Engine,
	event policy.Event,
	execute func() error,
) error {
	t.Helper()
	result := engine.Evaluate(context.Background(), event)
	switch result.Action {
	case policy.Deny:
		return errors.New(result.Reason)
	case policy.Ask:
		// The bridge has no way to park a call, so ask behaves as block-with-a-
		// reason; a turn-level ask needs the response flow.
		return errors.New("requires approval: " + result.Reason)
	}
	return execute()
}

// TestDenyPreventsExecution is the property that makes the engine a guardrail:
// a denied call must not reach the handler at all.
func TestDenyPreventsExecution(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{{
		Name:       "deny-bash",
		Tools:      []string{"bash"},
		Expression: "true",
		Action:     policy.Deny,
		Reason:     "shell access is not permitted",
		Enabled:    true,
	}})

	var executed bool
	err := runGated(t, engine, policy.Event{Type: policy.PhaseToolCall, Target: "bash"},
		func() error { executed = true; return nil })

	if err == nil {
		t.Fatal("a denied call returned no error")
	}
	if executed {
		t.Fatal("a denied call reached the handler; the gate is only a log line")
	}
	if err.Error() != "shell access is not permitted" {
		t.Errorf("error = %q, want the policy's reason", err)
	}
}

// TestAllowedCallExecutes covers the other half: the gate must not stop what it
// has no opinion about.
func TestAllowedCallExecutes(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{{
		Name:       "deny-bash",
		Tools:      []string{"bash"},
		Expression: "true",
		Action:     policy.Deny,
		Enabled:    true,
	}})

	var executed bool
	err := runGated(t, engine, policy.Event{Type: policy.PhaseToolCall, Target: "read"},
		func() error { executed = true; return nil })

	if err != nil {
		t.Fatalf("an ungated call failed: %v", err)
	}
	if !executed {
		t.Error("an ungated call did not reach the handler")
	}
}

// TestDenyBlocksEveryPathThroughTheGate: with several deny rules the handler must
// still never see the call.
func TestDenyBlocksEveryPathThroughTheGate(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{
		{Name: "deny-write", Tools: []string{"write", "omnigent_write_file"}, Expression: "true", Action: policy.Deny, Reason: "writes are frozen", Enabled: true},
		{Name: "deny-by-text", Expression: `event.text.contains("rm -rf")`, Action: policy.Deny, Reason: "destructive command", Enabled: true},
	})

	cases := []policy.Event{
		{Type: policy.PhaseToolCall, Target: "write"},
		{Type: policy.PhaseToolCall, Target: "omnigent_write_file"},
		{Type: policy.PhaseToolCall, Target: "bash", Text: "rm -rf /"},
	}
	for _, event := range cases {
		var executed bool
		err := runGated(t, engine, event, func() error { executed = true; return nil })
		if err == nil || executed {
			t.Errorf("event %+v: err=%v executed=%v, want a refusal and no execution",
				event, err, executed)
		}
	}
}

// TestGateIsConcurrencySafe: turns run in parallel goroutines, so the engine is
// read concurrently while its rules are compiled once.
func TestGateIsConcurrencySafe(t *testing.T) {
	engine := mustEngine(t, []policy.Rule{{
		Name:       "deny-bash",
		Tools:      []string{"bash"},
		Expression: `event.target == "bash"`,
		Action:     policy.Deny,
		Enabled:    true,
	}})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := "read"
			if i%2 == 0 {
				target = "bash"
			}
			result := engine.Evaluate(context.Background(), policy.Event{
				Type: policy.PhaseToolCall, Target: target,
			})
			want := policy.Allow
			if target == "bash" {
				want = policy.Deny
			}
			if result.Action != want {
				t.Errorf("target %s: action = %v, want %v", target, result.Action, want)
			}
		}(i)
	}
	wg.Wait()
}

// TestRulesRoundTripThroughJSON covers the environment-variable source the server
// reads: the JSON shape must decode into the rule struct.
func TestRulesRoundTripThroughJSON(t *testing.T) {
	raw := `[
	  {"name":"deny-bash","phases":["tool_call"],"tools":["bash"],
	   "expression":"true","action":"DENY","reason":"no shell","enabled":true}
	]`
	var rules []policy.Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		t.Fatalf("decode rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("decoded %d rules, want 1", len(rules))
	}
	engine, err := policy.NewEngine(rules)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	result := engine.Evaluate(context.Background(), policy.Event{
		Type: policy.PhaseToolCall, Target: "bash",
	})
	if result.Action != policy.Deny {
		t.Errorf("action = %v, want DENY after a JSON round trip", result.Action)
	}
}
