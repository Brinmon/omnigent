package server

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// composerContextLabelKey is the label family the client uses to send the
// composer's context (working directory, worktree mode) with a create.
//
// The value is chunked across `<key>.0`, `<key>.1`, ... because a label column is
// bounded; the chunks concatenate back into one JSON document.
const composerContextLabelKey = "omnigent.composer_context.v1.0"

// composerContext is the part of that document this build reads.
type composerContext struct {
	WorkingDirectory struct {
		Path string `json:"path"`
	} `json:"working_directory"`
	Worktree struct {
		Mode string `json:"mode"`
	} `json:"worktree"`
}

// workingDirectoryFromLabels recovers the workspace the user picked.
//
// The client sends it only inside the composer-context label -- the JSON create
// body has no workspace field -- so without this the session is created with no
// working directory and the composer shows "No workspace" after a create that the
// user had pointed at a folder.
//
// A malformed document is not fatal: the create still succeeds, it just runs in
// the default directory, which is what the Python server does too.
func workingDirectoryFromLabels(labels map[string]string) string {
	raw := joinComposerContext(labels)
	if raw == "" {
		return ""
	}
	var context composerContext
	if err := json.Unmarshal([]byte(raw), &context); err != nil {
		return ""
	}
	return strings.TrimSpace(context.WorkingDirectory.Path)
}

// joinComposerContext reassembles the chunked label value in index order.
//
// The indices are numeric and must be sorted numerically: sorting them as strings
// would place chunk 10 before chunk 2 and produce a document that does not parse.
func joinComposerContext(labels map[string]string) string {
	prefix := composerContextLabelKey + "."
	indices := make([]int, 0, len(labels))
	chunks := make(map[int]string, len(labels))
	for key, value := range labels {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil {
			continue
		}
		indices = append(indices, index)
		chunks[index] = value
	}
	if len(indices) == 0 {
		// A server that did not chunk the value stores it under the bare key.
		return labels[composerContextLabelKey]
	}
	sort.Ints(indices)

	var builder strings.Builder
	for _, index := range indices {
		builder.WriteString(chunks[index])
	}
	return builder.String()
}
