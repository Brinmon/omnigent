// Package db holds the on-disk encodings shared with the Python
// implementation. Every type here must stay byte-compatible: a database
// written by either implementation has to be readable by the other, both
// during the rewrite and after it if a user rolls back.
package db

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// UUID16Len is the stored width of a Uuid16 column.
const UUID16Len = 16

// Uuid16 converts between the bare 32-character hex form the rest of the
// system uses (entities, JSON, URLs) and the 16 raw bytes the column stores.
//
// The Python side accepts bare, dashed, and legacy plain-UUID forms on write
// and always returns bare lowercase hex on read; this mirrors that.
type Uuid16 [UUID16Len]byte

// ParseUuid16 accepts the forms the Python implementation accepts on write:
// 32 bare hex characters, the dashed 8-4-4-4-12 form, or a leading "uuid:"
// prefix. Anything else is an error, which callers map to a 404 the way the
// Python server maps InvalidUuidError.
func ParseUuid16(s string) (Uuid16, error) {
	var out Uuid16
	trimmed := strings.TrimSpace(s)
	trimmed = strings.TrimPrefix(trimmed, "uuid:")
	// Dashes are insignificant: the canonical form has four, and a UUID with
	// grouping dashes elsewhere still denotes the same 128 bits.
	trimmed = strings.ReplaceAll(trimmed, "-", "")
	if len(trimmed) != UUID16Len*2 {
		return out, fmt.Errorf("invalid uuid %q: want %d hex chars, got %d",
			s, UUID16Len*2, len(trimmed))
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return out, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	copy(out[:], raw)
	return out, nil
}

// MustParseUuid16 is ParseUuid16 for compile-time constants in tests.
func MustParseUuid16(s string) Uuid16 {
	id, err := ParseUuid16(s)
	if err != nil {
		panic(err)
	}
	return id
}

// String returns the bare lowercase hex form the API and entities use.
func (u Uuid16) String() string {
	return hex.EncodeToString(u[:])
}

// IsZero reports whether the value is the all-zero UUID.
func (u Uuid16) IsZero() bool {
	return u == Uuid16{}
}

// MarshalText makes Uuid16 encode as its hex string in JSON, matching the
// entity representation the API exposes.
func (u Uuid16) MarshalText() ([]byte, error) {
	return []byte(u.String()), nil
}

// UnmarshalText accepts the same forms ParseUuid16 does.
func (u *Uuid16) UnmarshalText(text []byte) error {
	parsed, err := ParseUuid16(string(text))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// ScanBlob converts a stored column value into a Uuid16. Drivers hand back
// []byte, string, or nil depending on the dialect and whether the value was
// written through an ORM, so all three shapes are accepted.
func ScanBlob(v any) (Uuid16, error) {
	var out Uuid16
	switch value := v.(type) {
	case nil:
		return out, fmt.Errorf("null value is not a uuid; use NullScanBlob for nullable columns")
	case []byte:
		return fromBytes(value)
	case string:
		// SQLite returns TEXT for a value written as text; the Python side
		// treats it as the hex form.
		return ParseUuid16(value)
	default:
		return out, fmt.Errorf("unsupported uuid column type %T", v)
	}
}

// NullScanBlob is ScanBlob for a nullable column, reporting presence.
func NullScanBlob(v any) (Uuid16, bool, error) {
	if v == nil {
		return Uuid16{}, false, nil
	}
	id, err := ScanBlob(v)
	return id, err == nil, err
}

func fromBytes(raw []byte) (Uuid16, error) {
	var out Uuid16
	if len(raw) == UUID16Len {
		copy(out[:], raw)
		return out, nil
	}
	// A hex string stored in a BLOB column: older rows and the FTS mirror
	// keep the readable form.
	if len(raw) == UUID16Len*2 {
		if id, err := ParseUuid16(string(raw)); err == nil {
			return id, nil
		}
	}
	return out, fmt.Errorf("invalid uuid bytes: want %d, got %d", UUID16Len, len(raw))
}
