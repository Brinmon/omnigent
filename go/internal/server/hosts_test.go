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
