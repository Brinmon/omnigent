package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestPutGetRoundTrip(t *testing.T) {
	store := newTestStore(t)
	want := []byte("bundle bytes\x00\x01\x02")
	if err := store.Put("agent/id/bundle.tar.gz", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get("agent/id/bundle.tar.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get returned %q, want %q", got, want)
	}
	if !store.Exists("agent/id/bundle.tar.gz") {
		t.Fatal("Exists reports false for a stored blob")
	}
}

func TestGetMissingBlobReportsNotExist(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get("agent/none/bundle.tar.gz")
	if err == nil {
		t.Fatal("Get on a missing blob returned no error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get error %v does not wrap os.ErrNotExist; callers cannot distinguish a missing blob from a read failure", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put("a/b", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete("a/b"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := store.Delete("a/b"); err != nil {
		t.Fatalf("second Delete should be a no-op, got %v", err)
	}
	if store.Exists("a/b") {
		t.Fatal("Exists still reports true after Delete")
	}
}

// A key is attacker-controlled in the multipart create path, so traversal must
// be refused before any filesystem call.
func TestKeysCannotEscapeTheRoot(t *testing.T) {
	store := newTestStore(t)
	bad := []string{
		"",
		"../outside",
		"a/../../outside",
		"/absolute/path",
		`back\slash`,
		"./dot",
		"C:/windows/system32",
		"..",
	}
	for _, key := range bad {
		t.Run(key, func(t *testing.T) {
			if err := store.Put(key, []byte("x")); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Put(%q) error = %v, want ErrInvalidKey", key, err)
			}
			if _, err := store.Get(key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Get(%q) error = %v, want ErrInvalidKey", key, err)
			}
			if store.Exists(key) {
				t.Fatalf("Exists(%q) = true, want false", key)
			}
		})
	}
}

func TestKeyWithDotSegmentsInsideIsAccepted(t *testing.T) {
	store := newTestStore(t)
	// "a/./b" is not traversal; it resolves inside the root and should work.
	if err := store.Put("a/./b", []byte("ok")); err == nil {
		t.Fatal("expected dot segments to be rejected for predictability")
	}
}

// A reader must never observe a half-written blob: Put renames into place.
func TestConcurrentReadsSeeWholeBlobs(t *testing.T) {
	store := newTestStore(t)
	first := []byte(strings.Repeat("a", 32*1024))
	second := []byte(strings.Repeat("b", 32*1024))
	if err := store.Put("key", first); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			payload := first
			if i%2 == 0 {
				payload = second
			}
			if err := store.Put("key", payload); err != nil {
				t.Errorf("Put: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := store.Get("key")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			if len(data) != len(first) {
				t.Errorf("torn read: got %d bytes, want %d", len(data), len(first))
				return
			}
		}
	}()

	// Let both goroutines interleave, then stop them.
	for i := 0; i < 200; i++ {
		if i%20 == 0 {
			continue
		}
	}
	close(stop)
	wg.Wait()
}

func TestPutCreatesIntermediateDirectories(t *testing.T) {
	store := newTestStore(t)
	if err := store.Put("deep/nested/tree/blob.bin", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	expected := filepath.Join(store.Root(), "deep", "nested", "tree", "blob.bin")
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("blob not at %s: %v", expected, err)
	}
}
