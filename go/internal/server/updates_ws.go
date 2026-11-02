package server

// The session-list update WebSocket.
//
// WS /v1/sessions/updates replaces the web app's HTTP poll of GET /v1/sessions
// with one persistent connection. It is not part of openapi.json (the document
// cannot describe a WebSocket handshake), so its protocol is defined by the
// Python implementation and reproduced here:
//
//	client -> server   {"type":"watch","session_ids":[...]}
//	                   The ids currently displayed. Sent on connect and again
//	                   whenever the visible set changes; it replaces the prior
//	                   set entirely. Unknown shapes are ignored so a newer
//	                   client can add messages without breaking this server.
//
//	server -> client   {"type":"snapshot","items":[...]}  after each watch
//	                   {"type":"changed","items":[...]}   watched rows changed
//	                   {"type":"removed","ids":[...]}     watched rows vanished
//	                   {"type":"heartbeat"}               nothing to send
//
// Freshness is pull-based: each rescan interval the server re-reads the watched
// ids and emits only what changed. That keeps the diff logic in one place
// (compare vs the last snapshot) rather than requiring every write path to
// publish.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/coder/websocket"

	"github.com/Brinmon/omnigent/go/internal/api"
	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// The Python server's timing constants, reproduced so a client's idle handling
// and a proxy's timeout behave identically against either build.
const (
	sessionUpdatesRescanInterval    = 4 * time.Second
	sessionUpdatesHeartbeatInterval = 30 * time.Second
	sessionUpdatesMaxWatched        = 500
)

// watchMessage is the only client frame this server understands.
type watchMessage struct {
	Type       string   `json:"type"`
	SessionIds []string `json:"session_ids"`
}

// updateFrame is one server frame. Only the fields for its Type are set.
type updateFrame struct {
	Type  string                `json:"type"`
	Items []api.SessionListItem `json:"items,omitempty"`
	Ids   []string              `json:"ids,omitempty"`
}

// handleSessionUpdates serves one client's watch-set.
func (s *Server) handleSessionUpdates(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The web client is served from the same origin as the API, so the
		// default same-origin check applies. A cross-origin embed would need an
		// explicit origin list rather than a wildcard.
		InsecureSkipVerify: false,
	})
	if err != nil {
		return // Accept already wrote a response
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// One goroutine reads client frames; the main loop writes. Splitting them is
	// required because both directions can block independently.
	watched := make(chan []string, 1)
	readErr := make(chan error, 1)
	go func() {
		readErr <- s.readWatchMessages(ctx, conn, watched)
	}()

	var (
		lastSent  = map[string]api.SessionListItem{}
		lastWrite = time.Now()
		watchSet  []string
	)

	rescan := time.NewTicker(sessionUpdatesRescanInterval)
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case err := <-readErr:
			// A clean close or a client going away is the normal exit.
			if err != nil && !isNormalClose(err) {
				conn.Close(websocket.StatusInternalError, "read failed")
			}
			return

		case ids := <-watched:
			watchSet = ids
			items, err := s.fetchWatchedItems(ctx, watchSet)
			if err != nil {
				// Keep the connection: the next tick retries, and a transient
				// read failure should not force a reconnect.
				continue
			}
			if err := s.sendFrame(ctx, conn, updateFrame{Type: "snapshot", Items: items}); err != nil {
				return
			}
			lastSent = indexItems(items)
			lastWrite = time.Now()

		case <-rescan.C:
			if watchSet == nil {
				// No watch yet: nothing to diff, but the client still needs to
				// know the connection is alive.
				if time.Since(lastWrite) >= sessionUpdatesHeartbeatInterval {
					if err := s.sendFrame(ctx, conn, updateFrame{Type: "heartbeat"}); err != nil {
						return
					}
					lastWrite = time.Now()
				}
				continue
			}

			items, err := s.fetchWatchedItems(ctx, watchSet)
			if err != nil {
				continue // retry next tick, per the Python server's behaviour
			}
			current := indexItems(items)

			changed := make([]api.SessionListItem, 0)
			for id, item := range current {
				previous, seen := lastSent[id]
				if !seen || !sameListItem(previous, item) {
					changed = append(changed, item)
				}
			}
			// Stable order so a test (and a log) can compare frames.
			sort.Slice(changed, func(i, j int) bool { return changed[i].Id < changed[j].Id })

			if len(changed) > 0 {
				if err := s.sendFrame(ctx, conn, updateFrame{Type: "changed", Items: changed}); err != nil {
					return
				}
				lastWrite = time.Now()
			}

			removed := make([]string, 0)
			for id := range lastSent {
				if _, present := current[id]; !present {
					removed = append(removed, id)
				}
			}
			sort.Strings(removed)
			if len(removed) > 0 {
				if err := s.sendFrame(ctx, conn, updateFrame{Type: "removed", Ids: removed}); err != nil {
					return
				}
				lastWrite = time.Now()
			}

			lastSent = current

			if time.Since(lastWrite) >= sessionUpdatesHeartbeatInterval {
				if err := s.sendFrame(ctx, conn, updateFrame{Type: "heartbeat"}); err != nil {
					return
				}
				lastWrite = time.Now()
			}
		}
	}
}

// readWatchMessages forwards each watch frame's id list to the writer loop.
func (s *Server) readWatchMessages(ctx context.Context, conn *websocket.Conn, out chan<- []string) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var message watchMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue // unknown shapes are ignored for forward compatibility
		}
		if message.Type != "watch" {
			continue
		}
		ids := dedupeWatched(message.SessionIds)
		select {
		case out <- ids:
		default:
			// A new watch supersedes any pending one; dropping the older value
			// is exactly right here.
		}
	}
}

// dedupeWatched normalises a watch-set: unknown ids dropped, duplicates
// removed, and the size capped so one client cannot ask the server to read an
// unbounded number of rows per tick.
func dedupeWatched(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, raw := range ids {
		// A malformed id can never match a row, so it is dropped rather than
		// returning an error the client cannot act on.
		id, err := db.ParseUuid16(raw)
		if err != nil {
			continue
		}
		key := id.String()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
		if len(unique) >= sessionUpdatesMaxWatched {
			break
		}
	}
	return unique
}

// fetchWatchedItems reads the current list-item view for each watched id.
func (s *Server) fetchWatchedItems(ctx context.Context, ids []string) ([]api.SessionListItem, error) {
	items := make([]api.SessionListItem, 0, len(ids))
	for _, raw := range ids {
		id, err := db.ParseUuid16(raw)
		if err != nil {
			continue
		}
		row, err := s.queries.GetSession(ctx, store.GetSessionParams{
			WorkspaceID: s.workspace,
			ID:          id,
		})
		if err != nil {
			// A row deleted while watched is reported by the diff as removed,
			// so a read miss here is expected rather than an error.
			continue
		}
		items = append(items, s.watchListItem(ctx, row))
	}
	return items, nil
}

// watchListItem renders one row for the updates stream.
//
// It starts from the same view GET /v1/sessions returns, so the client can
// replace a row from either source without a shape mismatch.
func (s *Server) watchListItem(ctx context.Context, row store.Conversation) api.SessionListItem {
	return s.sessionListItemFor(ctx, row)
}

// indexItems keys items by id for diffing.
func indexItems(items []api.SessionListItem) map[string]api.SessionListItem {
	index := make(map[string]api.SessionListItem, len(items))
	for _, item := range items {
		index[item.Id] = item
	}
	return index
}

// sameListItem compares the fields the sidebar renders.
//
// Comparing the whole struct by JSON would also work, but naming the fields
// keeps the intent explicit: a change here means the row should re-render.
func sameListItem(a, b api.SessionListItem) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(encodedA) == string(encodedB)
}

// sendFrame marshals and writes one frame.
func (s *Server) sendFrame(ctx context.Context, conn *websocket.Conn, frame updateFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	// A short write deadline keeps a stalled client from pinning the goroutine.
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

// isNormalClose reports whether an error is an ordinary disconnect.
func isNormalClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure ||
		status == websocket.StatusGoingAway ||
		status == websocket.StatusNoStatusRcvd
}
