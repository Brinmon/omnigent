// Package bundle reads an agent bundle uploaded by the web client.
//
// A bundle is a gzipped tar holding either a single omnigent YAML (the shape the
// client uploads) or a directory with config.yaml. Only the single-file shape is
// supported in V1, because that is what the client produces and the config.yaml
// shape carries a spec vocabulary this rewrite does not implement yet.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrUnsupported reports a bundle that is well-formed but uses a shape this
// build does not implement. The caller turns it into a client error naming what
// was found, because the uploader can fix it.
var ErrUnsupported = errors.New("unsupported bundle")

// Spec is the part of an agent bundle the create path needs.
type Spec struct {
	// Name is the agent's name, from the YAML's `name`.
	Name string
	// Harness is the execution harness, from `executor.harness`.
	Harness string
	// Model is the model the agent asks for, from `executor.model`. Empty means
	// the harness default.
	Model string
	// Prompt is the agent's system prompt.
	Prompt string
	// Description is optional operator-facing text.
	Description string
	// Skills lists the skills the bundle ships under skills/<dir>/SKILL.md.
	Skills []Skill
	// YAMLName is the archive member the spec was read from, kept so an error
	// can name the file.
	YAMLName string
}

// Skill is one skill the bundle ships.
type Skill struct {
	Name        string
	Description string
}

// maxBundleBytes bounds decompression so a small upload cannot expand without
// limit and exhaust memory. Agent bundles are kilobytes; this is generous.
const maxBundleBytes = 32 << 20

// Parse reads a gzipped tar and returns the single-file omnigent spec it holds.
func Parse(data []byte) (Spec, error) {
	files, err := extract(data)
	if err != nil {
		return Spec{}, err
	}

	if _, ok := files["config.yaml"]; ok {
		return Spec{}, fmt.Errorf(
			"%w: config.yaml bundles (spec_version: 1) are not supported; "+
				"upload a single omnigent YAML instead", ErrUnsupported)
	}

	// Match the Python rule: exactly one root-level .yaml/.yml that looks like an
	// omnigent spec. Zero means the upload is malformed; more than one is
	// ambiguous and the Python side refuses it too.
	var candidates []string
	for name := range files {
		if strings.Contains(name, "/") {
			continue
		}
		if !hasYAMLExtension(name) {
			continue
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return Spec{}, fmt.Errorf(
			"%w: no .yaml or .yml spec at the bundle root", ErrUnsupported)
	}
	if len(candidates) > 1 {
		return Spec{}, fmt.Errorf(
			"%w: %d YAML specs at the bundle root; exactly one is required",
			ErrUnsupported, len(candidates))
	}

	name := candidates[0]
	document, err := decodeYAML(files[name])
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", name, err)
	}
	// A spec_version key marks the native config.yaml vocabulary, which a root
	// file carries only by mistake.
	if _, ok := document["spec_version"]; ok {
		return Spec{}, fmt.Errorf(
			"%w: %s declares spec_version, which belongs in config.yaml",
			ErrUnsupported, name)
	}
	if _, ok := document["name"]; !ok {
		return Spec{}, fmt.Errorf("%w: %s is missing the required field name",
			ErrUnsupported, name)
	}
	if _, ok := document["prompt"]; !ok {
		return Spec{}, fmt.Errorf("%w: %s is missing the required field prompt",
			ErrUnsupported, name)
	}

	spec := Spec{YAMLName: name}
	spec.Name, _ = document["name"].(string)
	spec.Prompt, _ = document["prompt"].(string)
	spec.Description, _ = document["description"].(string)
	if spec.Name == "" {
		return Spec{}, fmt.Errorf("%w: %s has an empty name", ErrUnsupported, name)
	}

	// The compat shape nests the harness under executor. A bundle that omits it
	// would leave the session with no way to run, so require it here rather than
	// failing later when a turn starts.
	if executor, ok := document["executor"].(map[string]any); ok {
		spec.Harness, _ = executor["harness"].(string)
		spec.Model, _ = executor["model"].(string)
	}
	if spec.Harness == "" {
		return Spec{}, fmt.Errorf(
			"%w: %s is missing executor.harness, which selects how the agent runs",
			ErrUnsupported, name)
	}
	spec.Skills = readSkills(files)
	return spec, nil
}

// readSkills finds skills/<dir>/SKILL.md members and reads their front matter.
//
// A skill is a directory under skills/ holding a SKILL.md whose YAML header names
// it. Only the header is read: the body is prose the agent uses, not something the
// agent list displays.
func readSkills(files map[string][]byte) []Skill {
	skills := make([]Skill, 0)
	for name, body := range files {
		parts := strings.Split(name, "/")
		if len(parts) != 3 || parts[0] != "skills" || parts[2] != "SKILL.md" {
			continue
		}
		header := skillHeader(body)
		if header == nil {
			continue
		}
		skillName, _ := header["name"].(string)
		if skillName == "" {
			skillName = parts[1]
		}
		description, _ := header["description"].(string)
		skills = append(skills, Skill{Name: skillName, Description: description})
	}
	// A map iterates in random order, so the list is sorted to keep the response
	// stable across runs.
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// skillHeader reads the YAML front matter between the leading --- markers.
func skillHeader(body []byte) map[string]any {
	text := string(body)
	if !strings.HasPrefix(text, "---") {
		return nil
	}
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	var header map[string]any
	if err := yaml.Unmarshal([]byte(rest[:end]), &header); err != nil {
		return nil
	}
	return header
}

// extract returns archive members keyed by cleaned forward-slash path.
func extract(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzipped archive: %v", ErrUnsupported, err)
	}
	defer func() { _ = gz.Close() }()

	files := make(map[string][]byte)
	total := 0
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: reading archive: %v", ErrUnsupported, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name, err := memberName(header.Name)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}

		remaining := maxBundleBytes - total
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: bundle exceeds %d bytes uncompressed",
				ErrUnsupported, maxBundleBytes)
		}
		body, err := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		if err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrUnsupported, name, err)
		}
		total += len(body)
		if total > maxBundleBytes {
			return nil, fmt.Errorf("%w: bundle exceeds %d bytes uncompressed",
				ErrUnsupported, maxBundleBytes)
		}
		files[name] = body
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: the archive is empty", ErrUnsupported)
	}
	return files, nil
}

// memberName rejects archive members that would escape the extraction root.
//
// Nothing is extracted to disk here, but a traversal name is still a sign of a
// hostile upload and names an entry the rest of this file would otherwise trust.
func memberName(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	cleaned := path.Clean(strings.ReplaceAll(raw, `\`, "/"))
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." ||
		strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: archive member %q escapes the bundle root",
			ErrUnsupported, raw)
	}
	if strings.HasPrefix(cleaned, "./") {
		cleaned = cleaned[2:]
	}
	return cleaned, nil
}

func hasYAMLExtension(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

func decodeYAML(body []byte) (map[string]any, error) {
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: not a YAML mapping: %v", ErrUnsupported, err)
	}
	if document == nil {
		return nil, fmt.Errorf("%w: the YAML document is empty", ErrUnsupported)
	}
	return document, nil
}
