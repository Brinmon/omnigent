package event_test

// A4: the 56 ServerStreamEvent discriminator values are the contract between
// the server and the reused web client. Python, TypeScript, and the Go
// rewrite must agree exactly.
//
// The generated table is compared against openapi.json at run time rather than
// only at generation time, so a change to the Python schema that nobody
// regenerated fails `go test` instead of shipping a client/server mismatch.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Brinmon/omnigent/go/internal/event"
)

const specPath = "../../../openapi.json"

// streamEventUnion is the subset of openapi.json this test reads.
type streamEventUnion struct {
	Components struct {
		Schemas map[string]struct {
			OneOf         []map[string]string `json:"oneOf"`
			Discriminator struct {
				PropertyName string            `json:"propertyName"`
				Mapping      map[string]string `json:"mapping"`
			} `json:"discriminator"`
		} `json:"schemas"`
	} `json:"components"`
}

func loadUnion(t *testing.T) streamEventUnion {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec streamEventUnion
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	return spec
}

// TestStreamEventTypesMatchSpec is the golden check: the generated table must
// equal the discriminator mapping in the published document.
func TestStreamEventTypesMatchSpec(t *testing.T) {
	union, ok := loadUnion(t).Components.Schemas["ServerStreamEvent"]
	if !ok {
		t.Fatal("openapi.json has no ServerStreamEvent schema; the contract moved")
	}
	want := make([]string, 0, len(union.Discriminator.Mapping))
	for name := range union.Discriminator.Mapping {
		want = append(want, name)
	}
	slices.Sort(want)

	got := slices.Clone(event.StreamEventTypes)
	slices.Sort(got)

	if !slices.Equal(got, want) {
		for _, name := range want {
			if !slices.Contains(got, name) {
				t.Errorf("missing event type %q (in openapi.json, not in Go)", name)
			}
		}
		for _, name := range got {
			if !slices.Contains(want, name) {
				t.Errorf("extra event type %q (in Go, not in openapi.json)", name)
			}
		}
		t.Fatalf("event type sets differ: Go has %d, spec has %d", len(got), len(want))
	}
	if len(got) != event.StreamEventCount {
		t.Errorf("table has %d entries but StreamEventCount says %d",
			len(got), event.StreamEventCount)
	}
}

// TestStreamEventCountIs56 pins the number so a silent shrink is loud. The
// count comes from the published document, not from memory.
func TestStreamEventCountIs56(t *testing.T) {
	union, ok := loadUnion(t).Components.Schemas["ServerStreamEvent"]
	if !ok {
		t.Fatal("no ServerStreamEvent schema")
	}
	if got := len(union.Discriminator.Mapping); got != 56 {
		t.Fatalf("openapi.json now has %d event types, not 56; "+
			"regenerate with designs/go-rewrite/gen-stream-events.py and "+
			"update this assertion deliberately", got)
	}
	if event.StreamEventCount != 56 {
		t.Errorf("generated StreamEventCount = %d, want 56", event.StreamEventCount)
	}
}

// TestUnionIsInternallyConsistent asserts the document's oneOf list and its
// discriminator mapping name the same schemas. Drift between them means the
// client and the validator disagree about what is valid.
func TestUnionIsInternallyConsistent(t *testing.T) {
	union, ok := loadUnion(t).Components.Schemas["ServerStreamEvent"]
	if !ok {
		t.Fatal("no ServerStreamEvent schema")
	}

	fromOneOf := map[string]bool{}
	for _, entry := range union.OneOf {
		ref, ok := entry["$ref"]
		if !ok {
			continue
		}
		fromOneOf[lastSegment(ref)] = true
	}
	fromMapping := map[string]bool{}
	for _, target := range union.Discriminator.Mapping {
		fromMapping[lastSegment(target)] = true
	}
	if union.Discriminator.PropertyName != "type" {
		t.Errorf("discriminator property is %q, want %q",
			union.Discriminator.PropertyName, "type")
	}
	if len(fromOneOf) != len(fromMapping) {
		t.Fatalf("oneOf lists %d schemas, mapping names %d",
			len(fromOneOf), len(fromMapping))
	}
	for name := range fromOneOf {
		if !fromMapping[name] {
			t.Errorf("%s is in oneOf but not in the discriminator mapping", name)
		}
	}
	for name := range fromMapping {
		if !fromOneOf[name] {
			t.Errorf("%s is in the discriminator mapping but not in oneOf", name)
		}
	}
}

// TestEveryEventMapsToASchema checks the generated name->schema table covers
// every type, which is what lets later tests assert each payload shape.
func TestEveryEventMapsToASchema(t *testing.T) {
	union := loadUnion(t).Components.Schemas["ServerStreamEvent"]

	if len(event.StreamEventSchema) != len(event.StreamEventTypes) {
		t.Fatalf("StreamEventSchema has %d entries, StreamEventTypes has %d",
			len(event.StreamEventSchema), len(event.StreamEventTypes))
	}
	for _, name := range event.StreamEventTypes {
		schemaName, ok := event.StreamEventSchema[name]
		if !ok {
			t.Errorf("event %q has no schema mapping", name)
			continue
		}
		target, ok := union.Discriminator.Mapping[name]
		if !ok {
			t.Errorf("event %q is not in the spec mapping anymore", name)
			continue
		}
		if want := lastSegment(target); schemaName != want {
			t.Errorf("event %q maps to %q, spec says %q", name, schemaName, want)
		}
	}
}

// TestStreamEventTypesAreSortedDocuments the generator's ordering contract so
// a hand edit that disturbs it is caught.
func TestStreamEventTypesAreSortedDocuments(t *testing.T) {
	if !slices.IsSorted(event.StreamEventTypes) {
		t.Error("StreamEventTypes is not sorted; regenerate rather than hand-editing")
	}
	if slices.Contains(event.StreamEventTypes, "") {
		t.Error("StreamEventTypes contains an empty entry")
	}
	seen := map[string]bool{}
	for _, name := range event.StreamEventTypes {
		if seen[name] {
			t.Errorf("duplicate event type %q", name)
		}
		seen[name] = true
	}
}

func lastSegment(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}
	return filepath.Base(ref)
}
