package server

// Decoding a stored item payload into the generated union type.
//
// ConversationItem_Data exposes one From<Type> constructor per member and no
// generic setter, so the item type name selects the constructor. Routing through
// the generated types (rather than passing raw JSON through) is what makes the
// response match the document: a payload the model rejects is dropped instead of
// being sent as a shape the client cannot parse.

import (
	"encoding/json"
	"fmt"

	"github.com/Brinmon/omnigent/go/internal/api"
)

// decodeItemData parses a stored payload into the union for the given item type.
//
// The caller passes the enum name from internal/db, so an item type this build
// does not know never reaches here.
func decodeItemData(itemType string, payload []byte) (api.ConversationItem_Data, error) {
	var out api.ConversationItem_Data

	switch itemType {
	case "message":
		var value api.MessageData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromMessageData(value)

	case "function_call":
		var value api.FunctionCallData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromFunctionCallData(value)

	case "function_call_output":
		var value api.FunctionCallOutputData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromFunctionCallOutputData(value)

	case "error":
		var value api.ErrorData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromErrorData(value)

	case "reasoning":
		var value api.ReasoningData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromReasoningData(value)

	case "compaction":
		var value api.CompactionData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromCompactionData(value)

	case "native_tool":
		var value api.NativeToolData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromNativeToolData(value)

	case "resource_event":
		var value api.ResourceEventData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromResourceEventData(value)

	case "routing_decision":
		var value api.RoutingDecisionData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromRoutingDecisionData(value)

	case "slash_command":
		var value api.SlashCommandData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromSlashCommandData(value)

	case "terminal_command":
		var value api.TerminalCommandData
		if err := json.Unmarshal(payload, &value); err != nil {
			return out, err
		}
		return out, out.FromTerminalCommandData(value)

	default:
		return out, fmt.Errorf("no union member for item type %q", itemType)
	}
}
