package api_test

// The generated ServerInterface is what turns A1-A3 from a test into a compile
// error: every operation in openapi.json becomes a method the Go server must
// implement, so a missing route stops `go build` rather than shipping a
// silently dead endpoint.
//
// These tests pin that guarantee to the document, so the two cannot drift:
// regenerating after an API change updates the interface, and a stale
// generation fails here.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

const (
	generatedServer = "server.gen.go"
	specPath        = "../../../openapi.json"
	// wantOperations is the published operation count (A2). It is asserted
	// against openapi.json below, not trusted from memory.
	wantOperations = 154
)

func TestGeneratedInterfaceCoversEveryOperation(t *testing.T) {
	file := parseFile(t, generatedServer)
	methods := serverInterfaceMethods(t, file)

	if len(methods) != wantOperations {
		t.Fatalf("ServerInterface declares %d operations, want %d; "+
			"regenerate with `oapi-codegen -config oapi-server.yaml ../../../openapi.json`",
			len(methods), wantOperations)
	}

	// Every method must take the http.ResponseWriter first, which is what makes
	// the generated wrapper able to route to it at all.
	for name, params := range methods {
		if len(params) < 2 || params[0] != "w http.ResponseWriter" {
			t.Errorf("%s: signature does not start with an http.ResponseWriter", name)
		}
	}
}

// TestOperationCountMatchesSpec ties wantOperations to the document so this
// test does not silently keep passing after the API grows.
func TestOperationCountMatchesSpec(t *testing.T) {
	spec := loadSpec(t)
	count := 0
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json has no paths object")
	}
	for _, rawPath := range paths {
		item, ok := rawPath.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
			if _, ok := item[method]; ok {
				count++
			}
		}
	}
	if count != wantOperations {
		t.Fatalf("openapi.json declares %d operations, but this test expects %d; "+
			"regenerate the interface and update wantOperations deliberately", count, wantOperations)
	}
}

// TestGeneratedTypesCarryTheUnion proves the 56-variant ServerStreamEvent made
// it into the generated types at all, and that the variant structs exist so a
// caller can unmarshal into them.
//
// oapi-codegen models a “oneOf“ without a mapped discriminator as an opaque
// union (“type ServerStreamEvent struct { union json.RawMessage }“), which is
// correct here: the payload shape is selected by the “type“ field at runtime,
// and internal/event carries the name-to-schema mapping that A4 pins. What this
// test guards is the failure mode that pruning used to cause -- the union and
// every variant silently absent from the package.
func TestGeneratedTypesCarryTheUnion(t *testing.T) {
	file := parseFile(t, "types.gen.go")

	declared := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if typeSpec, ok := n.(*ast.TypeSpec); ok {
			declared[typeSpec.Name.Name] = true
		}
		return true
	})

	if !declared["ServerStreamEvent"] {
		t.Fatal("types.gen.go does not declare ServerStreamEvent; " +
			"regenerate with oapi-codegen (skip-prune must stay true)")
	}

	// Every union member must exist as its own type, otherwise a caller cannot
	// decode a frame even though the union type is present.
	missing := make([]string, 0)
	for _, name := range streamEventSchemaNames(t) {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d union member type(s) missing from types.gen.go: %v",
			len(missing), missing)
	}
}

// streamEventSchemaNames reads the union's member schema names out of the spec,
// so this test follows the document rather than a copied list.
func streamEventSchemaNames(t *testing.T) []string {
	t.Helper()
	spec := loadSpec(t)
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json has no components object")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json has no components.schemas object")
	}
	union, ok := schemas["ServerStreamEvent"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json has no ServerStreamEvent schema")
	}
	discriminator, ok := union["discriminator"].(map[string]any)
	if !ok {
		t.Fatal("ServerStreamEvent has no discriminator")
	}
	mapping, ok := discriminator["mapping"].(map[string]any)
	if !ok {
		t.Fatal("ServerStreamEvent has no discriminator mapping")
	}

	names := make([]string, 0, len(mapping))
	for _, target := range mapping {
		ref, ok := target.(string)
		if !ok {
			continue
		}
		names = append(names, ref[strings.LastIndex(ref, "/")+1:])
	}
	return names
}

// parseFile parses a generated Go file in this package directory.
func parseFile(t *testing.T, name string) *ast.File {
	t.Helper()
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("missing %s; regenerate with oapi-codegen: %v", name, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// serverInterfaceMethods returns each declared operation method and its
// parameter text, keyed by method name.
func serverInterfaceMethods(t *testing.T, file *ast.File) map[string][]string {
	t.Helper()
	methods := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		typeSpec, ok := n.(*ast.TypeSpec)
		if !ok || typeSpec.Name.Name != "ServerInterface" {
			return true
		}
		iface, ok := typeSpec.Type.(*ast.InterfaceType)
		if !ok {
			t.Fatalf("ServerInterface is %T, want an interface", typeSpec.Type)
		}
		for _, field := range iface.Methods.List {
			funcType, ok := field.Type.(*ast.FuncType)
			if !ok {
				continue // embedded interface or type constraint
			}
			var params []string
			if funcType.Params != nil {
				for _, param := range funcType.Params.List {
					typeText := exprString(param.Type)
					if len(param.Names) == 0 {
						params = append(params, typeText)
						continue
					}
					for _, name := range param.Names {
						params = append(params, name.Name+" "+typeText)
					}
				}
			}
			for _, name := range field.Names {
				methods[name.Name] = params
			}
		}
		return false
	})
	return methods
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.Ident:
		return e.Name
	case *ast.ArrayType:
		return "[]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	default:
		return "<expr>"
	}
}

// loadSpec reads openapi.json as generic JSON: this test cares about operation
// counts, not about the typed model the generator produces.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	return spec
}
