// Package policy evaluates Omnigent policies against an event.
//
// A policy is a predicate over one enforcement point. Three decisions are
// possible, and the difference between them is operational, not cosmetic:
//
//	ALLOW  proceed
//	ASK    park the call until a human answers
//	DENY   refuse, and do not run the tool
//
// The engine composes policies with a fixed precedence rather than a vote, since
// an ALLOW from one policy must not override a DENY from another: a deny-list
// that any allow-list can outvote provides no protection.
package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
)

// Action is a policy decision.
type Action string

const (
	// Allow proceeds with the action.
	Allow Action = "ALLOW"
	// Ask parks the call until a human answers (an ASK card).
	Ask Action = "ASK"
	// Deny refuses the call. The action does not run.
	Deny Action = "DENY"
)

// Phase is an enforcement point.
//
// The wire vocabulary is the lower-case form; “PHASE_REQUEST“ and
// “PHASE_TOOL_CALL“ are how the rest of Omnigent names two of them.
type Phase string

const (
	// PhaseRequest fires before a model turn.
	PhaseRequest Phase = "request"
	// PhaseToolCall fires before a tool runs.
	PhaseToolCall Phase = "tool_call"
	// PhaseToolResult fires after a tool returns.
	PhaseToolResult Phase = "tool_result"
	// PhaseResponse fires after a model turn.
	PhaseResponse Phase = "response"
)

// Event is the value a policy is evaluated against.
type Event struct {
	// Type is the enforcement point.
	Type Phase
	// Target is the tool or resource the phase concerns, e.g. "bash". Empty for
	// a request phase, which has no target.
	Target string
	// Text is the prompt or tool argument text a policy may inspect.
	Text string
	// Data carries additional fields, exposed to CEL as ``event.data``.
	Data map[string]any
}

// Result is one policy's decision.
type Result struct {
	Action Action
	// Reason is shown to the user on ASK and recorded on DENY; empty on ALLOW.
	Reason string
	// Policy is the policy that decided.
	Policy string
}

// Rule is one policy.
type Rule struct {
	// Name identifies the policy in a Result and in a log line.
	Name string
	// Phases are the enforcement points this rule applies to. Empty means all.
	Phases []Phase
	// Tools restricts the rule to these tool names. Empty means any.
	Tools []string
	// Expression is a CEL predicate over ``event``. A true result yields Action.
	Expression string
	// Action is the decision a true expression produces.
	Action Action
	// Reason is reported when the expression holds.
	Reason string
	// Enabled is false for a disabled rule, which is skipped.
	Enabled bool
}

// compiledRule is a rule with its CEL program ready.
type compiledRule struct {
	rule    Rule
	program cel.Program
}

// Engine evaluates rules in order and composes their decisions.
type Engine struct {
	mu    sync.RWMutex
	rules []compiledRule
}

// NewEngine compiles rules into an engine.
//
// Compilation happens once so evaluation does not pay for parsing on every call;
// a rule that fails to compile is an error rather than a rule that silently never
// fires, because a policy that quietly stops applying is the worst outcome for a
// guardrail.
func NewEngine(rules []Rule) (*Engine, error) {
	env, err := cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("policy: build environment: %w", err)
	}

	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if strings.TrimSpace(rule.Expression) == "" {
			return nil, fmt.Errorf("policy %q: no expression", rule.Name)
		}
		ast, issues := env.Compile(rule.Expression)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("policy %q: %w", rule.Name, issues.Err())
		}
		if ast.OutputType() != cel.BoolType {
			return nil, fmt.Errorf("policy %q: expression yields %s, want bool",
				rule.Name, ast.OutputType())
		}
		program, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("policy %q: build program: %w", rule.Name, err)
		}
		// A rule with no action is a deny: the safe default for a guardrail is to
		// stop the action, not to let it through.
		if rule.Action == "" {
			rule.Action = Deny
		}
		compiled = append(compiled, compiledRule{rule: rule, program: program})
	}
	return &Engine{rules: compiled}, nil
}

// Empty reports whether the engine has no rules.
func (e *Engine) Empty() bool {
	if e == nil {
		return true
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.rules) == 0
}

// Rules returns the rule names, for diagnostics.
func (e *Engine) Rules() []string {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.rules))
	for _, compiled := range e.rules {
		names = append(names, compiled.rule.Name)
	}
	sort.Strings(names)
	return names
}

// Evaluate returns the composed decision for an event.
//
// Precedence is DENY > ASK > ALLOW: the most restrictive decision among the
// applicable rules wins. An evaluation error is treated as a DENY for the same
// reason a guardrail fails closed -- an expression that cannot be evaluated must
// not be the one that lets an action through.
func (e *Engine) Evaluate(ctx context.Context, event Event) Result {
	if e == nil {
		return Result{Action: Allow}
	}
	e.mu.RLock()
	rules := e.rules
	e.mu.RUnlock()

	if len(rules) == 0 {
		return Result{Action: Allow}
	}

	var (
		deny *Result
		ask  *Result
	)
	for _, compiled := range rules {
		if !compiled.rule.appliesTo(event) {
			continue
		}
		matched, err := compiled.matches(ctx, event)
		if err != nil {
			// Fail closed: the rule could not be judged, so it must not be the
			// reason an action proceeds.
			return Result{
				Action: Deny,
				Reason: fmt.Sprintf("policy %q could not be evaluated: %v", compiled.rule.Name, err),
				Policy: compiled.rule.Name,
			}
		}
		if !matched {
			continue
		}
		result := Result{
			Action: compiled.rule.Action,
			Reason: compiled.rule.Reason,
			Policy: compiled.rule.Name,
		}
		switch result.Action {
		case Deny:
			if deny == nil {
				deny = &result
			}
		case Ask:
			if ask == nil {
				ask = &result
			}
		}
	}

	// Deny outranks ask: if any policy refuses, the human should not be asked to
	// approve something another policy already forbids.
	if deny != nil {
		return *deny
	}
	if ask != nil {
		return *ask
	}
	return Result{Action: Allow}
}

// appliesTo reports whether the rule's phase and tool filters admit an event.
func (r Rule) appliesTo(event Event) bool {
	if len(r.Phases) > 0 {
		found := false
		for _, phase := range r.Phases {
			if phase == event.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.Tools) > 0 {
		// A tool-scoped rule never matches a request phase, which has no target.
		if event.Target == "" {
			return false
		}
		found := false
		for _, tool := range r.Tools {
			if tool == event.Target {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// matches runs the CEL program against the event.
func (c compiledRule) matches(ctx context.Context, event Event) (bool, error) {
	activation := map[string]any{"event": event.toCEL()}
	outcome, _, err := c.program.ContextEval(ctx, activation)
	if err != nil {
		return false, err
	}
	matched, ok := outcome.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression yielded %T, want bool", outcome.Value())
	}
	return matched, nil
}

// toCEL renders the event for evaluation.
//
// The keys are the ones a policy author writes: “event.type“ distinguishes the
// phase and “event.target“ names the tool, matching the Python implementation's
// vocabulary so a rule written for one build applies to the other.
func (e Event) toCEL() map[string]any {
	fields := map[string]any{
		"type":   string(e.Type),
		"target": e.Target,
		"text":   e.Text,
	}
	// The open-ended bag is exposed under its own key rather than flattened, so a
	// rule reading ``event.data.x`` cannot collide with the reserved fields above.
	data := make(map[string]any, len(e.Data))
	for key, value := range e.Data {
		data[key] = value
	}
	fields["data"] = data
	return fields
}
