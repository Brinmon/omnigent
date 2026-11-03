package server

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Brinmon/omnigent/go/internal/api"
)

// defaultListingLimit matches the picker's request.
const defaultListingLimit = 200

// ListHostFilesystemRootV1HostsHostIdFilesystemGet lists the host's home
// directory, which is where the workspace picker opens.
func (s *Server) ListHostFilesystemRootV1HostsHostIdFilesystemGet(
	w http.ResponseWriter, r *http.Request, hostID string,
	params api.ListHostFilesystemRootV1HostsHostIdFilesystemGetParams) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not resolve the home directory: "+err.Error())
		return
	}
	s.listHostDirectory(w, hostID, home, params.Limit)
}

// listHostFilesystemPath serves the wildcard route the client actually calls.
//
// It exists because the document declares a single-segment {path} while the
// client sends an absolute path with separators in it. The wildcard captures the
// rest of the URL, which is then decoded segment by segment.
func (s *Server) listHostFilesystemPath(w http.ResponseWriter, r *http.Request) {
	hostID := pathParam(r, "host_id")
	// chi hands back the still-encoded remainder, so "C%3A/Users/me" must be
	// decoded before it can name a directory. Each segment is decoded separately
	// because a whole-string decode would also turn an encoded slash inside a
	// segment into a path separator.
	raw := chi.URLParam(r, "*")
	if raw == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	decoded, err := decodePathSegments(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input",
			"malformed path: "+err.Error())
		return
	}
	s.listHostDirectory(w, hostID, restoreAbsolutePath(decoded), queryListingLimit(r))
}

// decodePathSegments percent-decodes each segment of a captured wildcard path.
func decodePathSegments(raw string) (string, error) {
	segments := strings.Split(raw, "/")
	for index, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return "", err
		}
		segments[index] = decoded
	}
	return strings.Join(segments, "/"), nil
}

// queryListingLimit reads ?limit=, defaulting when absent or malformed.
func queryListingLimit(r *http.Request) *int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return nil
	}
	return &parsed
}

// ListHostFilesystemV1HostsHostIdFilesystemPathGet lists one directory.
func (s *Server) ListHostFilesystemV1HostsHostIdFilesystemPathGet(
	w http.ResponseWriter, r *http.Request, hostID, path string,
	params api.ListHostFilesystemV1HostsHostIdFilesystemPathGetParams) {
	// The route strips the leading slash, so it is restored here; the client
	// documents that asymmetry (useHostFilesystem.ts).
	target := restoreAbsolutePath(path)
	s.listHostDirectory(w, hostID, target, params.Limit)
}

// listHostDirectory answers a directory listing for the local host.
//
// The Python server forwards this to a connected runner over its websocket and
// answers 409 when the host is offline. This build runs the harness in-process,
// so the filesystem is simply the one this process can read -- there is no
// forwarding step, and no offline case to report.
func (s *Server) listHostDirectory(w http.ResponseWriter, hostID, dir string, limit *int) {
	if hex.EncodeToString(localHostID()) != hostID {
		writeError(w, http.StatusNotFound, "not_found", "Host not found.")
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found",
				"Directory not found: "+filepath.ToSlash(dir))
			return
		}
		// A permission failure on one directory must not read as a server fault:
		// the picker treats 403 as "cannot browse here" and moves on.
		if os.IsPermission(err) {
			writeError(w, http.StatusForbidden, "forbidden",
				"Cannot read "+filepath.ToSlash(dir))
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error",
			"could not read "+filepath.ToSlash(dir))
		return
	}

	max := defaultListingLimit
	if limit != nil && *limit > 0 {
		max = *limit
	}

	// Directories first, then by name: the picker lists them in the order the
	// user expects to browse, and a stable order keeps the UI from reshuffling.
	sort.SliceStable(entries, func(i, j int) bool {
		dirI, dirJ := entries[i].IsDir(), entries[j].IsDir()
		if dirI != dirJ {
			return dirI
		}
		return entries[i].Name() < entries[j].Name()
	})

	data := make([]map[string]any, 0, len(entries))
	hasMore := false
	for _, entry := range entries {
		if !includeInListing(entry) {
			continue
		}
		if len(data) >= max {
			hasMore = true
			break
		}
		info, err := entry.Info()
		if err != nil {
			// An entry that vanished between ReadDir and Info is skipped rather
			// than failing the whole listing.
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if !info.Mode().IsRegular() {
			kind = "other"
		}
		var bytes any
		if kind == "file" {
			bytes = info.Size()
		}
		data = append(data, map[string]any{
			"name":        entry.Name(),
			"path":        filepath.ToSlash(filepath.Join(dir, entry.Name())),
			"type":        kind,
			"bytes":       bytes,
			"modified_at": info.ModTime().Unix(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "list",
		"data":     data,
		"has_more": hasMore,
	})
}

// restoreAbsolutePath turns a route segment back into an absolute path.
//
// A POSIX path lost its leading slash to the route and gets it back. A Windows
// path such as "C:/Users/me" is already absolute -- prepending a slash produced
// "/C:/Users/me", which no filesystem has, and the picker reported the user's own
// home as missing.
func restoreAbsolutePath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return string(filepath.Separator)
	}
	// A drive letter or a UNC share is absolute on its own.
	if filepath.VolumeName(trimmed) != "" {
		return filepath.FromSlash(trimmed)
	}
	return filepath.FromSlash("/" + trimmed)
}

// includeInListing reports whether an entry belongs in a workspace picker.
//
// Dotfiles are hidden because a workspace is a project directory, not a place to
// browse .ssh or .git internals; the picker offers no way to reveal them either.
func includeInListing(entry os.DirEntry) bool {
	return !strings.HasPrefix(entry.Name(), ".")
}
