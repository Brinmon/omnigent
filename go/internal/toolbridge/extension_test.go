package toolbridge_test

// The generated extension runs inside Pi's process, so a syntax error there
// aborts Pi at startup and takes the whole turn with it. These tests check the
// source structurally and, when node is available, parse it for real.

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/Brinmon/omnigent/go/internal/toolbridge"
)

// TestGenerateExtensionIsValidJavaScript parses the output with node.
//
// A structural check would pass source that node rejects, and the failure would
// only appear when Pi loads it -- as "Failed to load extension", with no hint
// about which line.
func TestGenerateExtensionIsValidJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; cannot parse the generated extension")
	}

	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  54321,
		Token: "test-token-abc123",
		Tools: []toolbridge.ToolSpec{
			{
				Name:        "sys_os_read",
				Description: "Read a file from the workspace.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}

	// `node --check` parses without executing, which is exactly the load-time
	// check Pi performs.
	cmd := exec.Command(node, "--check", "-")
	cmd.Stdin = strings.NewReader(source)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node rejected the generated extension: %v\n%s\n--- source ---\n%s",
			err, output, source)
	}
}

// TestExtensionEmbedsPortAndToken checks the two values the extension needs to
// reach the bridge. A wrong port means every tool call fails to connect; a wrong
// token means every call is rejected as unauthorized.
func TestExtensionEmbedsPortAndToken(t *testing.T) {
	const token = "tok-with-\"quotes\"-and-\\backslash"
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  45678,
		Token: token,
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}

	if !strings.Contains(source, "const PORT = 45678;") {
		t.Error("the generated extension does not embed the port")
	}
	// The token is JSON-encoded, so a quote in it must be escaped rather than
	// breaking the string literal.
	if !strings.Contains(source, `const TOKEN = "tok-with-\"quotes\"-and-\\backslash";`) {
		t.Errorf("the token is not correctly escaped for a JS literal:\n%s",
			excerptAround(source, "const TOKEN"))
	}
}

// TestExtensionRegistersEveryTool covers the registration block: a tool missing
// here is a tool the model can never call.
func TestExtensionRegistersEveryTool(t *testing.T) {
	tools := []toolbridge.ToolSpec{
		{Name: "alpha", Description: "First."},
		{Name: "beta", Description: "Second."},
	}
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port: 1, Token: "t", Tools: tools,
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}
	for _, tool := range tools {
		if !strings.Contains(source, `"name": "`+tool.Name+`"`) {
			t.Errorf("tool %q is not in the generated descriptor list", tool.Name)
		}
	}
	// pi.registerTool must be called for each; the loop is over TOOLS, so the
	// presence of the call plus the descriptor list covers it.
	if !strings.Contains(source, "pi.registerTool({") {
		t.Error("the extension does not register tools with Pi")
	}
	if !strings.Contains(source, "const BRIDGED = new Set(TOOLS.map((t) => t.name));") {
		t.Error("the extension does not build the bridged-name set the hook needs")
	}
}

// TestExtensionHookSkipsBridgedTools is the double-prompt guard.
//
// A bridged tool is evaluated when its call reaches the bridge; if the
// tool_call hook also evaluated it, the user would be asked twice for one call.
func TestExtensionHookSkipsBridgedTools(t *testing.T) {
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port: 1, Token: "t",
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}
	// The guard must be present and must reference BRIDGED before evaluating.
	hookIndex := strings.Index(source, `pi.on("tool_call"`)
	if hookIndex < 0 {
		t.Fatal("the extension has no tool_call hook; native tools would escape policy")
	}
	hook := source[hookIndex:]
	guardIndex := strings.Index(hook, "if (BRIDGED.has(event.toolName)) return;")
	if guardIndex < 0 {
		t.Fatal("the hook does not skip bridged tools, so a bridged call would be evaluated twice")
	}
	evalIndex := strings.Index(hook, "evalNativePolicy(")
	if evalIndex < 0 {
		t.Fatal("the hook never asks for a verdict")
	}
	if guardIndex > evalIndex {
		t.Error("the bridged-tool guard runs after the policy evaluation")
	}
}

// TestExtensionEmptyToolListStillGates covers a turn with no bridged tools: the
// hook must still generate, because Pi's native tools are exactly what needs
// gating in that case.
func TestExtensionEmptyToolListStillGates(t *testing.T) {
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port: 1234, Token: "t",
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}
	if !strings.Contains(source, `pi.on("tool_call"`) {
		t.Error("an empty tool list dropped the policy hook")
	}
	if !strings.Contains(source, "const TOOLS = [];") {
		t.Errorf("the descriptor list is not an empty array:\n%s", excerptAround(source, "const TOOLS"))
	}
}

// TestExtensionSkipsUnnamedTools keeps one malformed spec from poisoning the
// whole extension: Pi rejects an unnamed tool at load time.
func TestExtensionSkipsUnnamedTools(t *testing.T) {
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  1,
		Token: "t",
		Tools: []toolbridge.ToolSpec{
			{Name: "", Description: "no name"},
			{Name: "   ", Description: "blank name"},
			{Name: "good", Description: "fine"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}
	if strings.Contains(source, "no name") || strings.Contains(source, "blank name") {
		t.Error("an unnamed tool was emitted; Pi would reject the extension at load time")
	}
	if !strings.Contains(source, `"name": "good"`) {
		t.Error("the valid tool was dropped along with the invalid ones")
	}
}

// TestExtensionDefaultsMissingSchema covers a tool declared without parameters:
// Pi requires an object schema, and an absent one breaks registration.
func TestExtensionDefaultsMissingSchema(t *testing.T) {
	source, err := toolbridge.GenerateExtension(toolbridge.ExtensionOptions{
		Port:  1,
		Token: "t",
		Tools: []toolbridge.ToolSpec{{Name: "noargs", Description: "takes nothing"}},
	})
	if err != nil {
		t.Fatalf("GenerateExtension: %v", err)
	}
	if !strings.Contains(source, `"parameters": {`) {
		t.Error("a tool without parameters did not receive the default object schema")
	}
	if !strings.Contains(source, `"properties": {}`) {
		t.Errorf("the default schema is not an empty object:\n%s", excerptAround(source, "noargs"))
	}
}

// excerptAround returns a window of source around the first occurrence of needle,
// so a failure message shows the relevant block rather than the whole file.
func excerptAround(source, needle string) string {
	index := strings.Index(source, needle)
	if index < 0 {
		return "(needle not found)"
	}
	start := index - 120
	if start < 0 {
		start = 0
	}
	end := index + 240
	if end > len(source) {
		end = len(source)
	}
	return source[start:end]
}
