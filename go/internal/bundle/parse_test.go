package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"testing"
)

// archive packs the given members into a gzipped tar, the shape the client
// uploads.
func archive(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for name, body := range members {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buffer.Bytes()
}

// This is the exact bundle the web client uploads: a single YAML with the
// shorthand executor block, not a config.yaml.
const clientBundle = `name: probe_agent
description: Minimal agent.
executor:
  model: deepseek-v4.1-flash
  harness: pi
prompt: |
  You are a minimal probe agent. Answer briefly.
`

func TestParseClientBundle(t *testing.T) {
	spec, err := Parse(archive(t, map[string]string{"probe_agent.yaml": clientBundle}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Name != "probe_agent" {
		t.Errorf("Name = %q, want probe_agent", spec.Name)
	}
	if spec.Harness != "pi" {
		t.Errorf("Harness = %q, want pi", spec.Harness)
	}
	if spec.Model != "deepseek-v4.1-flash" {
		t.Errorf("Model = %q, want deepseek-v4.1-flash", spec.Model)
	}
	if spec.Description != "Minimal agent." {
		t.Errorf("Description = %q", spec.Description)
	}
	if spec.YAMLName != "probe_agent.yaml" {
		t.Errorf("YAMLName = %q", spec.YAMLName)
	}
	if spec.Prompt == "" {
		t.Error("Prompt is empty")
	}
}

// A file called config.yaml takes the native spec_version parser in the Python
// build and fails there with "missing required field: spec_version". Refusing it
// explicitly is friendlier than silently treating it as a root YAML.
func TestConfigYAMLIsRejectedWithGuidance(t *testing.T) {
	_, err := Parse(archive(t, map[string]string{"config.yaml": clientBundle}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("config.yaml")) {
		t.Errorf("error %q does not name config.yaml", err)
	}
}

func TestMissingHarnessIsRejected(t *testing.T) {
	body := "name: a\nprompt: hi\nexecutor:\n  model: m\n"
	_, err := Parse(archive(t, map[string]string{"a.yaml": body}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("executor.harness")) {
		t.Errorf("error %q does not name executor.harness", err)
	}
}

func TestMissingPromptIsRejected(t *testing.T) {
	body := "name: a\nexecutor:\n  harness: pi\n"
	_, err := Parse(archive(t, map[string]string{"a.yaml": body}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestSpecVersionAtRootIsRejected(t *testing.T) {
	body := "spec_version: 1\nname: a\nprompt: hi\nexecutor:\n  harness: pi\n"
	_, err := Parse(archive(t, map[string]string{"a.yaml": body}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestNoYAMLAtRootIsRejected(t *testing.T) {
	_, err := Parse(archive(t, map[string]string{"readme.txt": "hi"}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestTwoYAMLAtRootIsRejected(t *testing.T) {
	_, err := Parse(archive(t, map[string]string{
		"a.yaml": clientBundle,
		"b.yaml": clientBundle,
	}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("exactly one")) {
		t.Errorf("error %q does not explain the ambiguity", err)
	}
}

// A YAML in a subdirectory does not qualify, matching the Python rule that only
// looks at the root.
func TestYAMLInSubdirectoryIsNotASpec(t *testing.T) {
	_, err := Parse(archive(t, map[string]string{"sub/a.yaml": clientBundle}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestNotAGzipIsRejected(t *testing.T) {
	if _, err := Parse([]byte("plain text")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestArchiveMemberTraversalIsRejected(t *testing.T) {
	_, err := Parse(archive(t, map[string]string{"../escape.yaml": clientBundle}))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("escapes")) {
		t.Errorf("error %q does not mention escaping", err)
	}
}

func TestEmptyArchiveIsRejected(t *testing.T) {
	if _, err := Parse(archive(t, map[string]string{})); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestYmlExtensionIsAccepted(t *testing.T) {
	spec, err := Parse(archive(t, map[string]string{"agent.yml": clientBundle}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Name != "probe_agent" {
		t.Errorf("Name = %q", spec.Name)
	}
}
