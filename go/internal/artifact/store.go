// Package artifact stores agent bundles as blobs on the local filesystem.
//
// It is a Go port of the Python LocalArtifactStore, and it keeps the two
// properties that matter there: a key cannot escape the root, and a concurrent
// reader never observes a half-written file.
package artifact

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrInvalidKey reports a key that is empty, absolute, or tries to leave the
// root. Callers translate it into a client error rather than a server error,
// because a bad key came from the request.
var ErrInvalidKey = errors.New("invalid artifact key")

// Store is a root directory of flat blobs addressed by forward-slash keys.
type Store struct {
	root string
}

// NewStore prepares root as the blob directory, creating it if needed.
func NewStore(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact root %s: %w", root, err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact root %s: %w", root, err)
	}
	return &Store{root: absolute}, nil
}

// Root reports the absolute directory blobs are stored under.
func (s *Store) Root() string { return s.root }

// resolve maps a forward-slash key to an absolute path, rejecting traversal.
//
// The containment check runs after resolving so a symlink inside the root
// cannot redirect a write outside it.
func (s *Store) resolve(key string) (string, error) {
	if key == "" || strings.Contains(key, `\`) || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	// An absolute Windows path such as C:/x/../.. is not caught by the prefix
	// check above, so test for a volume name as well.
	if filepath.IsAbs(key) || filepath.VolumeName(key) != "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." || part == "." {
			return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}

	joined := filepath.Join(s.root, filepath.FromSlash(path.Clean(key)))
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve artifact key %q: %w", key, err)
	}
	relative, err := filepath.Rel(s.root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the root", ErrInvalidKey, key)
	}
	return resolved, nil
}

// Put stores data under key, replacing any existing blob.
//
// The bytes are written to a sibling temp file and renamed into place so a
// concurrent Get observes either the whole old blob or the whole new one.
func (s *Store) Put(key string, data []byte) error {
	target, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create artifact directory for %q: %w", key, err)
	}

	temp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp blob for %q: %w", key, err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp blob for %q: %w", key, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close temp blob for %q: %w", key, err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("rename blob into place for %q: %w", key, err)
	}
	return nil
}

// Get reads the blob stored under key.
func (s *Store) Get(key string) ([]byte, error) {
	target, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact %q: %w", key, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read artifact %q: %w", key, err)
	}
	return data, nil
}

// Delete removes the blob under key, succeeding when it is already absent.
func (s *Store) Delete(key string) error {
	target, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete artifact %q: %w", key, err)
	}
	return nil
}

// Exists reports whether a blob is stored under key.
func (s *Store) Exists(key string) bool {
	target, err := s.resolve(key)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(target)
	return statErr == nil
}
