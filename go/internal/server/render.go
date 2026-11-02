package server

// Rendering the session detail response.
//
// The generated api.SessionResponse marks optional fields `omitempty`, so a nil
// pointer disappears from the JSON, while the Python server serialises every
// field and sends `null`. The difference is observable -- a client testing
// `'active_response_id' in body` gets opposite answers -- so the detail is
// rendered through maps that keep the document's field names and add the nulls.
//
// ConversationItem.Data is likewise an opaque union in the generated types, with
// no generic setter, so the item payloads are carried as raw JSON and filled the
// same way.

import (
	"encoding/json"

	"github.com/Brinmon/omnigent/go/internal/api"
)

// sessionDetailBody is the rendered form of api.SessionResponse.
type sessionDetailBody map[string]json.RawMessage

// newSessionDetailBody marshals the generated struct so every populated field
// keeps the document's name and encoding, then fills the omitted optional keys
// with null.
func newSessionDetailBody(detail api.SessionResponse) (sessionDetailBody, error) {
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	var body sessionDetailBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	for _, key := range sessionResponseFields {
		if _, present := body[key]; !present {
			body[key] = json.RawMessage("null")
		}
	}
	return body, nil
}

// setItems replaces the items entry with payloads whose optional keys are
// present.
//
// The generated ConversationItem_Data has no generic setter, so the items are
// rebuilt here from the typed envelope plus the stored payload.
func (b sessionDetailBody) setItems(items []renderedItem) error {
	encoded, err := json.Marshal(items)
	if err != nil {
		return err
	}
	b["items"] = encoded
	return nil
}

// renderedItem is one transcript row: the generated envelope with its payload
// carried as raw JSON so the optional keys can be filled.
type renderedItem struct {
	Id         string          `json:"id"`
	CreatedAt  int64           `json:"created_at"`
	CreatedBy  *string         `json:"created_by"`
	Data       json.RawMessage `json:"data"`
	ResponseId string          `json:"response_id"`
	Status     string          `json:"status"`
	Type       string          `json:"type"`
}

// MarshalJSON emits the item's own keys, filling optional ones with null.
func (r renderedItem) MarshalJSON() ([]byte, error) {
	payload := r.Data
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	filled, err := fillItemNulls(r.Type, payload)
	if err != nil {
		filled = payload
	}
	fields := map[string]json.RawMessage{}
	// The envelope's own optional key (created_by) is filled alongside the
	// payload's, because Python emits it too.
	if r.CreatedBy != nil {
		encoded, err := json.Marshal(*r.CreatedBy)
		if err != nil {
			return nil, err
		}
		fields["created_by"] = encoded
	} else {
		fields["created_by"] = json.RawMessage("null")
	}
	for key, value := range map[string]any{
		"id":          r.Id,
		"created_at":  r.CreatedAt,
		"response_id": r.ResponseId,
		"status":      r.Status,
		"type":        r.Type,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	fields["data"] = filled
	return json.Marshal(fields)
}
