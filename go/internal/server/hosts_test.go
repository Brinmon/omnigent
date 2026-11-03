package server

import (
	"encoding/hex"
	"testing"
)

// The client remembers which host the user picked and re-selects it by id, so the
// id must be the same after a restart. A random id would silently reset that
// choice on every boot, which looks like the picker forgetting the user.
func TestLocalHostIDIsStable(t *testing.T) {
	first := localHostID()
	second := localHostID()
	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Fatalf("localHostID is not stable across calls: %x vs %x", first, second)
	}
	if len(first) != 16 {
		t.Fatalf("localHostID returned %d bytes, want 16", len(first))
	}
}

func TestPiAvailabilityReflectsTheEntry(t *testing.T) {
	if (piConfig{}).available() {
		t.Error("a piConfig with no entry reports available")
	}
	if (piConfig{reason: "not installed"}).available() {
		t.Error("a piConfig with a reason reports available")
	}
	if !(piConfig{entry: []string{"node", "cli.js"}}).available() {
		t.Error("a piConfig with an entry does not report available")
	}
}

// The client sends the picked directory only inside the composer-context label,
// chunked across indexed keys. Losing it leaves the session running in the
// default directory while the composer shows "No workspace".
func TestWorkingDirectoryFromLabels(t *testing.T) {
	document := `{"version":1,"working_directory":{"path":"C:/Users/me/project"},"worktree":{"mode":"none"}}`

	t.Run("single chunk", func(t *testing.T) {
		got := workingDirectoryFromLabels(map[string]string{
			composerContextLabelKey + ".0": document,
		})
		if got != "C:/Users/me/project" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unchunked value", func(t *testing.T) {
		got := workingDirectoryFromLabels(map[string]string{
			composerContextLabelKey: document,
		})
		if got != "C:/Users/me/project" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("chunks reassemble in numeric order", func(t *testing.T) {
		// Ten chunks force a two-digit index; sorting those as strings would put
		// ".10" before ".2" and produce a document that does not parse.
		const size = 10
		labels := map[string]string{}
		for offset, index := 0, 0; offset < len(document); offset, index = offset+size, index+1 {
			end := offset + size
			if end > len(document) {
				end = len(document)
			}
			labels[composerContextLabelKey+"."+itoa(index)] = document[offset:end]
		}
		if got := workingDirectoryFromLabels(labels); got != "C:/Users/me/project" {
			t.Fatalf("got %q from %d chunks", got, len(labels))
		}
	})

	t.Run("absent", func(t *testing.T) {
		if got := workingDirectoryFromLabels(map[string]string{"other": "x"}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if got := workingDirectoryFromLabels(map[string]string{
			composerContextLabelKey + ".0": "not json",
		}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}
