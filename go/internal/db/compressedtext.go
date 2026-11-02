package db

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// On-disk framing for CompressedText columns, shared with the Python
// implementation (see omnigent/db/compression.py).
//
// Layout: [sentinel][codec][payload]
//
//	sentinel 0x00 -- marks a framed value. Valid text in these columns can
//	                 never start with NUL (PostgreSQL forbids it in text, and
//	                 the JSON they hold leads with '{', '[' or '"'), so its
//	                 presence is an unambiguous "this row is framed" marker.
//	codec    0x00 -- payload is plain UTF-8
//	codec    0x01 -- payload is a zstd frame
//
// Rows written before the column became a BLOB are unframed UTF-8 with no
// sentinel (or, under SQLite's dynamic typing, arrive as TEXT). They decode
// unchanged and re-frame themselves on their next write, so no backfill is
// needed -- and that means every read path here must tolerate them.
const (
	compressionSentinel byte = 0x00
	codecRaw            byte = 0x00
	codecZstd           byte = 0x01

	// minCompressBytes is the point at which zstd's frame overhead stops
	// outweighing the gain. Below it the value is framed but left plain, so
	// the stored bytes stay identical to what Python would write.
	minCompressBytes = 64

	// compressionLevel favours ratio: these columns are write-once and
	// read-rarely, and the payloads are small enough that the window a high
	// level implies never fills.
	compressionLevel = 19
)

// ErrTruncatedFrame reports a framed value too short to hold a codec byte.
var ErrTruncatedFrame = errors.New("truncated compression frame")

// UnknownCodecError reports a frame whose codec id this build does not know.
// Reading it as text would corrupt the row, so it is an error.
type UnknownCodecError struct {
	Codec byte
}

func (e *UnknownCodecError) Error() string {
	return fmt.Sprintf("unknown compression codec 0x%02x", e.Codec)
}

// EncodeCompressedText frames a string for storage.
//
// It must produce byte-identical output to Python's “encode“: a row written
// here is read by a Python process if the user rolls back, and the two
// implementations write into the same column.
func EncodeCompressedText(text *string) ([]byte, error) {
	if text == nil {
		return nil, nil
	}
	raw := []byte(*text)
	if len(raw) < minCompressBytes {
		out := make([]byte, 0, len(raw)+2)
		out = append(out, compressionSentinel, codecRaw)
		return append(out, raw...), nil
	}
	packed, err := zstdFrame(raw)
	if err != nil {
		return nil, fmt.Errorf("compress text: %w", err)
	}
	out := make([]byte, 0, len(packed)+2)
	out = append(out, compressionSentinel, codecZstd)
	return append(out, packed...), nil
}

// DecodeCompressedText unframes a stored value, passing legacy unframed text
// through unchanged.
//
// The column arrives as []byte, string, or nil depending on dialect and on
// when the row was written, so all three shapes are accepted.
func DecodeCompressedText(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		// SQLite is dynamically typed: a value written before the column
		// became a BLOB comes back as TEXT. It is legacy plaintext.
		return v, nil
	case []byte:
		return decodeBytes(v)
	default:
		return "", fmt.Errorf("unsupported compressed-text column type %T", value)
	}
}

// NullDecodeCompressedText is DecodeCompressedText reporting presence, for
// nullable columns where NULL and "" differ.
func NullDecodeCompressedText(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	text, err := DecodeCompressedText(value)
	if err != nil {
		return nil, err
	}
	return &text, nil
}

func decodeBytes(value []byte) (string, error) {
	// Empty, or legacy UTF-8 text: a valid value cannot begin with NUL, so a
	// missing sentinel identifies the pre-migration format.
	if len(value) == 0 || value[0] != compressionSentinel {
		return string(value), nil
	}
	if len(value) < 2 {
		return "", ErrTruncatedFrame
	}
	codec, payload := value[1], value[2:]
	switch codec {
	case codecRaw:
		return string(payload), nil
	case codecZstd:
		plain, err := zstdUnframe(payload)
		if err != nil {
			return "", fmt.Errorf("decompress text: %w", err)
		}
		return string(plain), nil
	default:
		return "", &UnknownCodecError{Codec: codec}
	}
}

// zstdFrame compresses with the same parameters Python's “zstandard“ uses at
// level 19, so a row written here decodes there and vice versa. The frames do
// not have to be byte-identical (zstd frames are self-describing), but the
// encoder settings must agree on the plaintext they produce.
func zstdFrame(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(compressionLevel)),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw, nil), nil
}

func zstdUnframe(payload []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(payload, nil)
}
