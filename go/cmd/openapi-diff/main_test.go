package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The diff tool is the guardrail for A1-A3, so it has to be tested against
// deliberately broken documents. A tool that reports "no differences" no
// matter what would pass a same-vs-same smoke test and fail the rewrite
// silently, which is the failure mode this whole P0 exists to prevent.

const baselineSpec = "../../../openapi.json"

func loadBaseline(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(baselineSpec)
	if err != nil {
		t.Fatalf("read baseline %s: %v", baselineSpec, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	return spec
}

// writeMutated applies mutate to a copy of the baseline and returns its path.
func writeMutated(t *testing.T, mutate func(spec map[string]any)) string {
	t.Helper()
	// Reload each time: mutate must not see another test's changes.
	spec := loadBaseline(t)
	mutate(spec)
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal mutant: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mutant.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write mutant: %v", err)
	}
	return path
}

func mustLoad(t *testing.T, path string) *specification {
	t.Helper()
	spec, err := load(path, 0)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return spec
}

// TestIdenticalDocumentsMatch is the floor: the baseline must compare equal to
// itself, which also asserts the parser sees all 117/154/194 items.
func TestIdenticalDocumentsMatch(t *testing.T) {
	baseline := mustLoad(t, baselineSpec)
	candidate := mustLoad(t, baselineSpec)

	r := compare(baseline, candidate)
	if r.failures != 0 || r.warnings != 0 {
		t.Fatalf("self-comparison reported %d failures and %d warnings:\n%s",
			r.failures, r.warnings, renderLines(r))
	}
	if r.pathsMatched != 117 {
		t.Errorf("matched %d paths, want 117", r.pathsMatched)
	}
	if r.opsMatched != 154 {
		t.Errorf("matched %d operations, want 154", r.opsMatched)
	}
	if r.schemasMatched != 194 {
		t.Errorf("matched %d schemas, want 194", r.schemasMatched)
	}
}

// TestMissingPathFails is A1.
func TestMissingPathFails(t *testing.T) {
	mutant := writeMutated(t, func(spec map[string]any) {
		paths := spec["paths"].(map[string]any)
		delete(paths, "/v1/sessions/{session_id}")
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatal("deleting a path did not fail")
	}
	if !containsSubstring(renderLines(r), "missing path /v1/sessions/{session_id}") {
		t.Errorf("failure does not name the missing path:\n%s", renderLines(r))
	}
}

// TestMissingOperationFails is A2: the path stays, one method disappears.
func TestMissingOperationFails(t *testing.T) {
	mutant := writeMutated(t, func(spec map[string]any) {
		paths := spec["paths"].(map[string]any)
		item := paths["/v1/sessions/{session_id}"].(map[string]any)
		delete(item, "patch")
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatal("deleting a PATCH operation did not fail")
	}
	if !containsSubstring(renderLines(r), "missing PATCH /v1/sessions/{session_id}") {
		t.Errorf("failure does not name the missing operation:\n%s", renderLines(r))
	}
}

// TestMissingSchemaFails is A3 for a whole schema.
func TestMissingSchemaFails(t *testing.T) {
	mutant := writeMutated(t, func(spec map[string]any) {
		components := spec["components"].(map[string]any)
		schemas := components["schemas"].(map[string]any)
		delete(schemas, "ServerStreamEvent")
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatal("deleting ServerStreamEvent did not fail")
	}
	if !containsSubstring(renderLines(r), "missing schema ServerStreamEvent") {
		t.Errorf("failure does not name the missing schema:\n%s", renderLines(r))
	}
}

// TestMissingPropertyFails is A3 at field granularity: the Web client binds to
// property names, and a dropped one renders as a blank rather than an error.
func TestMissingPropertyFails(t *testing.T) {
	const target = "CompletedEvent"
	mutant := writeMutated(t, func(spec map[string]any) {
		components := spec["components"].(map[string]any)
		schemas := components["schemas"].(map[string]any)
		schema, ok := schemas[target].(map[string]any)
		if !ok {
			t.Fatalf("fixture lacks %s", target)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", target)
		}
		for name := range props {
			delete(props, name)
			break
		}
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatalf("dropping a %s property did not fail", target)
	}
	if !containsSubstring(renderLines(r), "missing property") {
		t.Errorf("failure does not mention the property:\n%s", renderLines(r))
	}
}

// TestRelaxedRequiredFails is A3 for required-ness.
func TestRelaxedRequiredFails(t *testing.T) {
	const target = "SessionStatusEvent"
	mutant := writeMutated(t, func(spec map[string]any) {
		components := spec["components"].(map[string]any)
		schemas := components["schemas"].(map[string]any)
		schema, ok := schemas[target].(map[string]any)
		if !ok {
			t.Fatalf("fixture lacks %s", target)
		}
		if _, ok := schema["required"]; !ok {
			t.Fatalf("%s has no required list to relax", target)
		}
		delete(schema, "required")
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatalf("removing the required list from %s did not fail", target)
	}
	if !containsSubstring(renderLines(r), "no longer required") {
		t.Errorf("failure does not mention required:\n%s", renderLines(r))
	}
}

// TestMissingEnumValueFails is A3 for enums.
func TestMissingEnumValueFails(t *testing.T) {
	changed := mutateFindEnum(t, func(values []any) ([]any, bool) {
		return values[:len(values)-1], true
	})
	if changed == "" {
		t.Fatal("baseline has no enum to shrink; the fixture assumption is stale")
	}
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, changed))
	if r.failures == 0 {
		t.Fatal("removing an enum value did not fail")
	}
	if !containsSubstring(renderLines(r), "missing enum value") {
		t.Errorf("failure does not mention the enum:\n%s", renderLines(r))
	}
}

// TestMissingUnionMemberFails is A4's precondition: ServerStreamEvent is the
// 56-variant union the web client switches on, so a dropped member is a
// missing UI state.
func TestMissingUnionMemberFails(t *testing.T) {
	mutant := writeMutated(t, func(spec map[string]any) {
		components := spec["components"].(map[string]any)
		schemas := components["schemas"].(map[string]any)
		schema := schemas["ServerStreamEvent"].(map[string]any)
		members, ok := schema["oneOf"].([]any)
		if !ok || len(members) < 2 {
			t.Fatalf("ServerStreamEvent has no oneOf list")
		}
		schema["oneOf"] = members[:len(members)-1]
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures == 0 {
		t.Fatal("removing a union member did not fail")
	}
	if !containsSubstring(renderLines(r), "missing member") {
		t.Errorf("failure does not mention the member:\n%s", renderLines(r))
	}
}

// TestExtraPathWarnsNotFails records the asymmetry: an addition cannot break a
// client that does not know about it, so it is a warning.
func TestExtraPathWarnsNotFails(t *testing.T) {
	mutant := writeMutated(t, func(spec map[string]any) {
		paths := spec["paths"].(map[string]any)
		paths["/v1/go-only-experiment"] = map[string]any{
			"get": map[string]any{"responses": map[string]any{"200": map[string]any{}}},
		}
	})
	r := compare(mustLoad(t, baselineSpec), mustLoad(t, mutant))
	if r.failures != 0 {
		t.Errorf("an extra path failed; additions should warn:\n%s", renderLines(r))
	}
	if r.warnings == 0 {
		t.Error("an extra path did not warn")
	}
}

// mutateFindEnum rewrites the first property-level enum found in the baseline
// so the enum tests do not hardcode a schema name that may change. Pydantic
// emits enums on properties, not on whole schemas.
func mutateFindEnum(t *testing.T, change func([]any) ([]any, bool)) string {
	t.Helper()
	var replaced string
	path := writeMutated(t, func(spec map[string]any) {
		components := spec["components"].(map[string]any)
		schemas := components["schemas"].(map[string]any)
		for _, schemaName := range sortedKeys(schemas) {
			schema, ok := schemas[schemaName].(map[string]any)
			if !ok {
				continue
			}
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				continue
			}
			for _, propName := range sortedKeys(props) {
				prop, ok := props[propName].(map[string]any)
				if !ok {
					continue
				}
				values, ok := prop["enum"].([]any)
				if !ok || len(values) < 2 {
					continue
				}
				next, ok := change(values)
				if !ok {
					continue
				}
				prop["enum"] = next
				replaced = schemaName + "." + propName
				return
			}
		}
	})
	return pathIfChanged(t, path, replaced)
}

func pathIfChanged(t *testing.T, path, replaced string) string {
	t.Helper()
	if replaced == "" {
		return ""
	}
	t.Logf("mutated enum on schema %s", replaced)
	return path
}

func renderLines(r *report) string {
	var b strings.Builder
	for _, line := range r.lines {
		b.WriteString(line.level)
		b.WriteString(" [")
		b.WriteString(line.area)
		b.WriteString("] ")
		b.WriteString(line.detail)
		b.WriteString("\n")
	}
	return b.String()
}

func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
