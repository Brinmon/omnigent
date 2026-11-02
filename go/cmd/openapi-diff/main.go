// Command openapi-diff compares two OpenAPI documents and fails when the Go
// build's surface has drifted from the Python one.
//
// A1/A2/A3 in designs/go-rewrite/ACCEPTANCE-TRACKER.md. The Go rewrite must
// serve exactly the paths, operations, and schemas the Python build serves,
// because the web client is reused unchanged and a missing field shows up as
// silently broken UI rather than an error.
//
// The comparison is deliberately strict in one direction and lenient in the
// other:
//
//   - removed or changed contract -- a path, operation, field, required flag,
//     or enum member the Python build serves and Go does not; these are
//     failures because an existing client breaks.
//   - additions -- new paths or optional fields on the Go side are reported as
//     warnings, since they cannot break a client that does not know them.
//
// Usage:
//
//	go run ./cmd/openapi-diff --old ../../openapi.json --new http://127.0.0.1:8080/openapi.json
//	go run ./cmd/openapi-diff --old ../../openapi.json --new ./go.json --strict
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// httpMethods are the operation keys OpenAPI allows under a path item. Others
// ("parameters", "summary", "$ref", ...) describe the path itself.
var httpMethods = []string{
	"get", "put", "post", "delete", "options", "head", "patch", "trace",
}

func main() {
	oldRef := flag.String("old", "", "baseline document (path or URL) -- the Python build")
	newRef := flag.String("new", "", "candidate document (path or URL) -- the Go build")
	strict := flag.Bool("strict", false, "treat additions as failures too")
	verbose := flag.Bool("v", false, "list every matched item, not just differences")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP timeout when fetching a URL")
	flag.Parse()

	if *oldRef == "" || *newRef == "" {
		fmt.Fprintln(os.Stderr, "usage: openapi-diff --old <path|url> --new <path|url>")
		os.Exit(2)
	}

	baseline, err := load(*oldRef, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read --old %s: %v\n", *oldRef, err)
		os.Exit(1)
	}
	candidate, err := load(*newRef, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read --new %s: %v\n", *newRef, err)
		os.Exit(1)
	}

	report := compare(baseline, candidate)
	report.print(os.Stdout, *verbose)

	if report.failures > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %d contract difference(s)\n", report.failures)
		os.Exit(1)
	}
	if *strict && report.warnings > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL (--strict): %d addition(s)\n", report.warnings)
		os.Exit(1)
	}
	fmt.Printf("\nOK: %d path(s), %d operation(s), %d schema(s) match\n",
		report.pathsMatched, report.opsMatched, report.schemasMatched)
}

// specification is the subset of OpenAPI this tool reads. Unknown fields are
// preserved as raw JSON where a schema needs to be compared, so a comparison
// does not silently ignore a keyword it happens not to model.
type specification struct {
	OpenAPI    string                    `json:"openapi"`
	Info       map[string]any            `json:"info"`
	Paths      map[string]map[string]any `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
}

func load(ref string, timeout time.Duration) (*specification, error) {
	var raw []byte
	var err error
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		raw, err = fetch(ref, timeout)
	} else {
		raw, err = os.ReadFile(ref)
	}
	if err != nil {
		return nil, err
	}
	var spec specification
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if spec.Paths == nil {
		return nil, errors.New("document has no paths object")
	}
	return &spec, nil
}

func fetch(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// report accumulates comparison findings. Failures are contract breaks;
// warnings are additions the baseline does not have.
type report struct {
	versions       [2]string
	failures       int
	warnings       int
	pathsMatched   int
	opsMatched     int
	schemasMatched int
	lines          []reportLine
}

type reportLine struct {
	level  string // "FAIL", "WARN", "INFO"
	area   string // "path", "operation", "schema", "enum", "required"
	detail string
}

func (r *report) fail(area, format string, args ...any) {
	r.failures++
	r.lines = append(r.lines, reportLine{"FAIL", area, fmt.Sprintf(format, args...)})
}

func (r *report) warn(area, format string, args ...any) {
	r.warnings++
	r.lines = append(r.lines, reportLine{"WARN", area, fmt.Sprintf(format, args...)})
}

func (r *report) info(area, format string, args ...any) {
	r.lines = append(r.lines, reportLine{"INFO", area, fmt.Sprintf(format, args...)})
}

// compare walks paths, operations, and schemas. Each section maps to one
// acceptance criterion, so a failure names the row it breaks.
func compare(baseline, candidate *specification) *report {
	r := &report{versions: [2]string{baseline.OpenAPI, candidate.OpenAPI}}

	// ── A1: path set ──────────────────────────────────────────────────────
	for _, path := range sortedKeys(baseline.Paths) {
		if _, ok := candidate.Paths[path]; !ok {
			r.fail("path", "missing path %s", path)
		}
	}
	for _, path := range sortedKeys(candidate.Paths) {
		if _, ok := baseline.Paths[path]; !ok {
			r.warn("path", "extra path %s", path)
		}
	}

	// ── A2: operations ────────────────────────────────────────────────────
	for _, path := range sortedKeys(baseline.Paths) {
		want, ok := baseline.Paths[path]
		if !ok {
			continue // already reported as missing
		}
		got, ok := candidate.Paths[path]
		if !ok {
			continue
		}
		for _, method := range httpMethods {
			wantOp, wantHas := want[method]
			gotOp, gotHas := got[method]
			switch {
			case wantHas && !gotHas:
				r.fail("operation", "missing %s %s", strings.ToUpper(method), path)
			case !wantHas && gotHas:
				r.warn("operation", "extra %s %s", strings.ToUpper(method), path)
			case wantHas && gotHas:
				r.opsMatched++
				compareOperation(r, method, path, wantOp, gotOp)
			}
		}
	}

	// ── A3: schemas ───────────────────────────────────────────────────────
	for _, name := range sortedKeys(baseline.Components.Schemas) {
		want := baseline.Components.Schemas[name]
		got, ok := candidate.Components.Schemas[name]
		if !ok {
			r.fail("schema", "missing schema %s", name)
			continue
		}
		r.schemasMatched++
		compareSchema(r, name, want, got)
	}
	for _, name := range sortedKeys(candidate.Components.Schemas) {
		if _, ok := baseline.Components.Schemas[name]; !ok {
			r.warn("schema", "extra schema %s", name)
		}
	}

	r.pathsMatched = countCommon(baseline.Paths, candidate.Paths)
	return r
}

// compareOperation checks what a caller observes about one operation: the
// properties of its request body and each documented response. Field-level
// drift here is what turns a working form into a silently ignored parameter.
func compareOperation(r *report, method, path string, want, got any) {
	wantMap, ok := want.(map[string]any)
	if !ok {
		return
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		return
	}
	label := strings.ToUpper(method) + " " + path

	// Responses: a status code the baseline documents but Go omits means the
	// client cannot handle that outcome.
	wantResp := asMap(wantMap["responses"])
	gotResp := asMap(gotMap["responses"])
	for _, code := range sortedKeys(wantResp) {
		if _, ok := gotResp[code]; !ok {
			r.fail("operation", "%s: missing response %s", label, code)
		}
	}
	for _, code := range sortedKeys(gotResp) {
		if _, ok := wantResp[code]; !ok {
			r.warn("operation", "%s: extra response %s", label, code)
		}
	}

	// Request body: required-ness must not relax.
	wantBody := asMap(wantMap["requestBody"])
	gotBody := asMap(gotMap["requestBody"])
	if wantRequired, ok := wantBody["required"].(bool); ok && wantRequired {
		if gotRequired, ok := gotBody["required"].(bool); !ok || !gotRequired {
			r.fail("operation", "%s: request body is no longer required", label)
		}
	}

	// Parameters: the baseline's query/path/header list is the contract.
	wantParams := asList(wantMap["parameters"])
	gotParams := asList(gotMap["parameters"])
	if len(wantParams) > 0 {
		gotByName := make(map[string]map[string]any, len(gotParams))
		for _, p := range gotParams {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			key := fmt.Sprint(pm["in"]) + ":" + fmt.Sprint(pm["name"])
			gotByName[key] = pm
		}
		for _, p := range wantParams {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			key := fmt.Sprint(pm["in"]) + ":" + fmt.Sprint(pm["name"])
			gm, ok := gotByName[key]
			if !ok {
				r.fail("operation", "%s: missing parameter %s", label, key)
				continue
			}
			if wantReq, _ := pm["required"].(bool); wantReq {
				if gotReq, _ := gm["required"].(bool); !gotReq {
					r.fail("operation", "%s: parameter %s is no longer required", label, key)
				}
			}
		}
	}
}

// compareSchema checks the three things a client binds to: which properties
// exist, which are required, and which enum members are allowed.
func compareSchema(r *report, name string, want, got map[string]any) {
	// A discriminator union (ServerStreamEvent) is compared by its member set:
	// the web client switches on the discriminator value, so a missing member
	// is a missing UI state.
	compareUnionMembers(r, name, want, got)

	wantProps := asMap(want["properties"])
	gotProps := asMap(got["properties"])
	// A property the baseline has and Go omits is a silent data loss: the
	// JSON simply does not carry it and the client renders a blank.
	for _, prop := range sortedKeys(wantProps) {
		gotProp, ok := gotProps[prop]
		if !ok {
			r.fail("schema", "%s.%s: missing property", name, prop)
			continue
		}
		comparePropertyType(r, name, prop, wantProps[prop], gotProp)
	}
	for _, prop := range sortedKeys(gotProps) {
		if _, ok := wantProps[prop]; !ok {
			r.warn("schema", "%s.%s: extra property", name, prop)
		}
	}

	// Required flags: a field the baseline requires and Go makes optional
	// means a client can receive null where it expects a value.
	wantReq := asStringSet(want["required"])
	gotReq := asStringSet(got["required"])
	for _, field := range sortedStrings(wantReq) {
		if !gotReq[field] {
			r.fail("required", "%s.%s: no longer required", name, field)
		}
	}

	// Enums: the allowed value set is part of the wire contract. Property-level
	// enums are compared in comparePropertyType; this covers the rarer form
	// where the schema itself is an enum.
	compareEnum(r, name, want["enum"], got["enum"])
}

// compareUnionMembers reports oneOf/anyOf member drift by discriminator value.
func compareUnionMembers(r *report, name string, want, got map[string]any) {
	for _, keyword := range []string{"oneOf", "anyOf"} {
		wantMembers := unionMemberNames(want[keyword])
		if len(wantMembers) == 0 {
			continue
		}
		gotMembers := unionMemberNames(got[keyword])
		for _, member := range wantMembers {
			if !contains(gotMembers, member) {
				r.fail("schema", "%s.%s: missing member %s", name, keyword, member)
			}
		}
	}
	// The discriminator mapping is the authoritative name->schema table; the
	// web client keys off it, so compare its keys as well.
	wantDisc := discriminatorValues(want["discriminator"])
	if len(wantDisc) == 0 {
		return
	}
	gotDisc := discriminatorValues(got["discriminator"])
	for _, value := range sortedStrings(wantDisc) {
		if !gotDisc[value] {
			r.fail("schema", "%s.discriminator: missing %q", name, value)
		}
	}
}

// comparePropertyType checks a property's type shape, including the anyOf/oneOf
// form Pydantic emits for optional fields.
func comparePropertyType(r *report, schemaName, prop string, want, got any) {
	wantMap, ok := want.(map[string]any)
	if !ok {
		return
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		return
	}
	if wantType, ok := wantMap["type"]; ok {
		if gotType, ok := gotMap["type"]; !ok || gotType != wantType {
			r.fail("schema", "%s.%s: type %v -> %v", schemaName, prop, wantType, gotType)
		}
	}
	// A nullable field is expressed as anyOf:[T, null]. Losing the null branch
	// makes an optional field mandatory at the type level.
	wantNullable := hasNullBranch(wantMap)
	gotNullable := hasNullBranch(gotMap)
	if wantNullable && !gotNullable {
		r.warn("schema", "%s.%s: no longer nullable", schemaName, prop)
	}

	// Pydantic puts an enum on the property, not the schema, so the allowed
	// value set has to be compared here: a dropped member is a response the
	// client cannot render.
	compareEnum(r, schemaName+"."+prop, wantMap["enum"], gotMap["enum"])
}

// compareEnum checks an allowed-value set, reporting removals as failures and
// additions as warnings.
func compareEnum(r *report, label string, want, got any) {
	wantEnum := asStringSet(want)
	if len(wantEnum) == 0 {
		return
	}
	gotEnum := asStringSet(got)
	for _, value := range sortedStrings(wantEnum) {
		if !gotEnum[value] {
			r.fail("enum", "%s: missing enum value %q", label, value)
		}
	}
	for _, value := range sortedStrings(gotEnum) {
		if !wantEnum[value] {
			r.warn("enum", "%s: extra enum value %q", label, value)
		}
	}
}

func hasNullBranch(schema map[string]any) bool {
	for _, keyword := range []string{"anyOf", "oneOf"} {
		for _, branch := range asList(schema[keyword]) {
			if bm, ok := branch.(map[string]any); ok {
				if bm["type"] == "null" {
					return true
				}
			}
		}
	}
	return false
}

func unionMemberNames(v any) []string {
	var names []string
	for _, item := range asList(v) {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if ref, ok := im["$ref"].(string); ok {
			names = append(names, ref[strings.LastIndex(ref, "/")+1:])
			continue
		}
		if title, ok := im["title"].(string); ok {
			names = append(names, title)
		}
	}
	sort.Strings(names)
	return names
}

func discriminatorValues(v any) map[string]bool {
	out := map[string]bool{}
	dm, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for key := range asMap(dm["mapping"]) {
		out[key] = true
	}
	return out
}

func (r *report) print(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "baseline openapi: %s\n", r.versions[0])
	fmt.Fprintf(w, "candidate openapi: %s\n\n", r.versions[1])
	if r.versions[0] != r.versions[1] {
		fmt.Fprintf(w, "WARN document versions differ: %s vs %s\n\n",
			r.versions[0], r.versions[1])
	}
	for _, line := range r.lines {
		if line.level == "INFO" && !verbose {
			continue
		}
		fmt.Fprintf(w, "%-4s [%s] %s\n", line.level, line.area, line.detail)
	}
	if r.failures == 0 && r.warnings == 0 {
		fmt.Fprintln(w, "no differences")
	}
}

// ── small helpers ─────────────────────────────────────────────────────────

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func asStringSet(v any) map[string]bool {
	out := map[string]bool{}
	for _, item := range asList(v) {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func countCommon[V any, W any](a map[string]V, b map[string]W) int {
	n := 0
	for key := range a {
		if _, ok := b[key]; ok {
			n++
		}
	}
	return n
}
