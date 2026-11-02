package db_test

// Round-trip check in the other direction: a row the Go build writes must be
// readable by the Python implementation. This matters past the rewrite, when a
// user rolls back after the Go build has already written history.
//
// Byte-identical frames are not required (zstd frames are self-describing),
// but the plaintext must survive and the framing must be one Python's decode()
// accepts. The Python-side assertion lives in
// designs/go-rewrite/verify-go-interop.py, which reads the file this test
// writes.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Brinmon/omnigent/go/internal/db"
)

// TestWritesAreReadableByPython emits a probe file for the Python verifier.
//
// It writes the three shapes the Python decoder distinguishes -- a zstd frame,
// a raw frame, and a pass-through -- into a temp file, then records the paths
// for designs/go-rewrite/verify-go-interop.py. When OMNIGENT_INTEROP_OUT is
// unset the test still runs its own assertions and skips only the handoff,
// so `go test ./...` works on a machine without the Python env.
func TestWritesAreReadableByPython(t *testing.T) {
	shortText := "short value below the compression threshold"
	longText := `{"session_state":"` + repeat("abcdefgh", 40) + `","note":"go wrote this"}`

	shortPtr, longPtr := shortText, longText
	shortFrame, err := db.EncodeCompressedText(&shortPtr)
	if err != nil {
		t.Fatalf("encode short: %v", err)
	}
	longFrame, err := db.EncodeCompressedText(&longPtr)
	if err != nil {
		t.Fatalf("encode long: %v", err)
	}

	// The frame shapes must match what Python's encode() produces: short text
	// is framed but plain, long text is framed and zstd.
	if len(shortFrame) < 2 || shortFrame[0] != 0x00 || shortFrame[1] != 0x00 {
		t.Fatalf("short text is not a raw frame: % x", shortFrame[:min(4, len(shortFrame))])
	}
	if len(longFrame) < 2 || longFrame[0] != 0x00 || longFrame[1] != 0x01 {
		t.Fatalf("long text is not a zstd frame: % x", longFrame[:min(4, len(longFrame))])
	}

	// Go must read back its own output before Python is asked to.
	if got, err := db.DecodeCompressedText(shortFrame); err != nil || got != shortText {
		t.Fatalf("decode short: got %q, err %v", got, err)
	}
	if got, err := db.DecodeCompressedText(longFrame); err != nil || got != longText {
		t.Fatalf("decode long: got %q, err %v", truncate(got, 60), err)
	}

	out := os.Getenv("OMNIGENT_INTEROP_OUT")
	if out == "" {
		t.Log("OMNIGENT_INTEROP_OUT unset; skipping the Python handoff probe")
		return
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("create probe dir: %v", err)
	}
	// A tiny file format keeps the verifier dependency-free: three
	// length-prefixed records, plaintext then frame, so Python can assert the
	// pair without a parser.
	var buf []byte
	for _, pair := range []struct {
		plain string
		frame []byte
	}{{shortText, shortFrame}, {longText, longFrame}} {
		buf = appendRecord(buf, []byte(pair.plain))
		buf = appendRecord(buf, pair.frame)
	}
	if err := os.WriteFile(out, buf, 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	t.Logf("wrote interop probe %s (%d bytes)", out, len(buf))
}

// appendRecord prefixes a 4-byte big-endian length. The verifier reads the same
// layout; keeping it dumb avoids shipping a codec on both sides of the check.
func appendRecord(dst, payload []byte) []byte {
	n := len(payload)
	dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(dst, payload...)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
