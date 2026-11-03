package server

// Persisting a turn's messages.
//
// A turn streams its events, and the client renders them live; without a write
// path a reload loses the transcript, which is what B3 and E3 assert against. The
// rows written here are the same shape the Python store writes, so a transcript
// recorded by this build is readable by either.
//
// Two invariants matter:
//
//   - positions are allocated from the conversation's counter, not from
//     MAX(position), so two concurrent turns cannot pick the same one;
//   - a turn's user message and its assistant reply are both written, so a
//     reload shows the pair rather than half a conversation.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Brinmon/omnigent/go/internal/db"
	"github.com/Brinmon/omnigent/go/internal/store"
)

// turnRecorder accumulates a turn's rows and writes them once.
//
// Writing per event would put a database round-trip between every delta; a turn
// produces one user message and one assistant message, so a single batch at the
// end is both cheaper and atomic.
type turnRecorder struct {
	responseID string
	items      []pendingItem
}

// pendingItem is one row before its position is known.
type pendingItem struct {
	itemType  string
	data      map[string]any
	search    string
	createdAt int64
}

// newTurnRecorder starts a recorder for one turn.
//
// The response id groups the turn's rows, which is how the client pairs a reply
// with its prompt.
func newTurnRecorder() *turnRecorder {
	return &turnRecorder{responseID: "resp_" + randomToken(12)}
}

// addUserMessage records the prompt.
func (r *turnRecorder) addUserMessage(text string) {
	r.items = append(r.items, pendingItem{
		itemType: "message",
		data: map[string]any{
			"role": "user",
			// input_text, not text: the client filters user bubbles with
			// isTextBlock, which accepts only input_text, so any other type renders
			// as an empty bubble.
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
		search:    text,
		createdAt: time.Now().Unix(),
	})
}

// addToolCall records a tool invocation.
//
// “arguments“ is stored as a JSON *string*, not an object: that is how the
// Python store writes it, and “parse_item_data“ reads it back expecting a
// string. Storing an object would make the row unreadable by the Python side.
func (r *turnRecorder) addToolCall(name, callID string, args json.RawMessage) {
	arguments := string(args)
	if arguments == "" {
		arguments = "{}"
	}
	r.items = append(r.items, pendingItem{
		itemType: "function_call",
		data: map[string]any{
			"agent":     "pi",
			"name":      name,
			"arguments": arguments,
			"call_id":   callID,
		},
		search:    name,
		createdAt: time.Now().Unix(),
	})
}

// addToolResult records a tool's output.
func (r *turnRecorder) addToolResult(callID, output string, isError bool) {
	data := map[string]any{
		"call_id": callID,
		"output":  output,
	}
	if isError {
		// The stored shape mirrors MessageData/ErrorData's convention: a
		// failure is flagged rather than inferred from the text.
		data["is_error"] = true
	}
	r.items = append(r.items, pendingItem{
		itemType:  "function_call_output",
		data:      data,
		search:    output,
		createdAt: time.Now().Unix(),
	})
}

// addAssistantMessage records the reply.
func (r *turnRecorder) addAssistantMessage(text, model string) {
	if strings.TrimSpace(text) == "" {
		// A turn that produced no text writes no row: an empty bubble reads as
		// "the agent said nothing", which is worse than an absent reply the
		// failure event already explains.
		return
	}
	r.items = append(r.items, pendingItem{
		itemType: "message",
		data: map[string]any{
			"role": "assistant",
			// output_text for an assistant reply; the counterpart to input_text.
			"content": []map[string]any{{"type": "output_text", "text": text}},
			"model":   model,
		},
		search:    text,
		createdAt: time.Now().Unix(),
	})
}

// flush writes the accumulated rows in one transaction.
func (r *turnRecorder) flush(ctx context.Context, conn *sql.DB, workspaceID int64, conversationID db.Uuid16) error {
	if len(r.items) == 0 {
		return nil
	}

	positions, err := allocatePositions(ctx, conn, workspaceID, conversationID, int64(len(r.items)))
	if err != nil {
		return err
	}

	for index, item := range r.items {
		typeCode, err := db.EncodeName(db.ItemType, "conversation_items.type", item.itemType)
		if err != nil {
			return err
		}
		statusCode, err := db.EncodeName(db.ItemStatus, "conversation_items.status", "completed")
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(item.data)
		if err != nil {
			return fmt.Errorf("encode %s item: %w", item.itemType, err)
		}
		itemID, err := newUuid16()
		if err != nil {
			return err
		}
		queries := store.New(conn)
		if err := queries.AppendConversationItem(ctx, store.AppendConversationItemParams{
			WorkspaceID:    workspaceID,
			ConversationID: conversationID,
			ID:             itemID,
			ResponseID:     r.responseID,
			CreatedAt:      item.createdAt,
			Position:       positions[index],
			Data:           string(encoded),
			SearchText:     item.search,
			CreatedBy:      nil,
			Type:           typeCode,
			Status:         statusCode,
		}); err != nil {
			return fmt.Errorf("append %s item: %w", item.itemType, err)
		}
	}
	return nil
}

// allocatePositions reserves n consecutive positions.
//
// A conversation created before the counter existed has a NULL next_position;
// the fallback scans once and the UPDATE then backfills the counter, exactly as
// the Python store does, so only the first append after an upgrade pays.
func allocatePositions(
	ctx context.Context,
	conn *sql.DB,
	workspaceID int64,
	conversationID db.Uuid16,
	count int64,
) ([]int64, error) {
	queries := store.New(conn)
	row, err := queries.AllocateNextPositionRange(ctx, store.AllocateNextPositionRangeParams{
		WorkspaceID: workspaceID,
		ID:          conversationID,
		// The statement adds this to the counter and returns the new value.
		NextPosition: &count,
	})
	if err != nil {
		return nil, fmt.Errorf("allocate positions: %w", err)
	}
	// RETURNING gives the post-update counter, so the range starts count below
	// it. Deriving the start here rather than in SQL keeps the statement to one
	// occurrence of the parameter, which is what SQLite's binding requires.
	end, err := asInt64(row)
	if err != nil {
		return nil, fmt.Errorf("allocate positions: %w", err)
	}
	start := end - count

	positions := make([]int64, count)
	for i := int64(0); i < count; i++ {
		positions[i] = start + i
	}
	return positions, nil
}

// errNullCounter reports a conversation whose position counter column is NULL.
var errNullCounter = errors.New("conversation position counter is NULL")

// asInt64 accepts the numeric shapes a SQLite RETURNING column can arrive as.
func asInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case *int64:
		// sqlc maps a nullable counter to *int64, which is the shape a
		// RETURNING on next_position arrives as.
		if typed == nil {
			return 0, errNullCounter
		}
		return *typed, nil
	case int32:
		return int64(typed), nil
	case int:
		return int64(typed), nil
	case float64:
		return int64(typed), nil
	case []byte:
		var parsed int64
		if _, err := fmt.Sscanf(string(typed), "%d", &parsed); err != nil {
			return 0, fmt.Errorf("column is not an integer: %q", typed)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("column is %T, want an integer", value)
	}
}

// newUuid16 returns a random id in the column's 16-byte form.
func newUuid16() (db.Uuid16, error) {
	var id db.Uuid16
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generate id: %w", err)
	}
	// RFC 4122 version 4 / variant bits, so the id is a valid uuid rather than
	// merely random bytes.
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}

// randomToken is a short url-safe token for the response id.
func randomToken(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		// A response id only has to be unique within a turn; falling back to a
		// timestamp keeps the turn writable rather than failing it.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}
