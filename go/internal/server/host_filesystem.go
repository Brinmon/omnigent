package server

import (
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// ListHostFilesystemV1HostsHostIdFilesystemPathGet lists one directory.
func (s *Server) ListHostFilesystemV1HostsHostIdFilesystemPathGet(
	w http.ResponseWriter, r *http.Request, hostID, path string,
	params api.ListHostFilesystemV1HostsHostIdFilesystemPathGetParams) {
	// The route strips the leading slash, so it is restored here; the client
	// documents that asymmetry (useHostFilesystem.ts).
	target := "/" + strings.TrimPrefix(path, "/")
	s.listHostDirectory(w, hostID, filepath.FromSlash(target), params.Limit)
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

// includeInListing reports whether an entry belongs in a workspace picker.
//
// Dotfiles are hidden because a workspace is a project directory, not a place to
// browse .ssh or .git internals; the picker offers no way to reveal them either.
func includeInListing(entry os.DirEntry) bool {
	return !strings.HasPrefix(entry.Name(), ".")
}
