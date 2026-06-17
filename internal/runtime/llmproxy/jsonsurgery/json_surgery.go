package jsonsurgery

import (
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/jsonpatch"
)

// FindFieldValue locates the byte range of a top-level field value in a
// JSON object. This compatibility package delegates to runtime/jsonpatch so
// all byte-faithful JSON surgery shares one implementation.
func FindFieldValue(data []byte, key string) (int, int, bool) {
	return jsonpatch.FindTopLevelFieldValue(data, key)
}

// SetField replaces or appends a top-level JSON object field.
func SetField(data []byte, key string, newValue []byte) ([]byte, error) {
	return jsonpatch.SetTopLevelField(data, key, newValue)
}

// DeleteField removes a top-level JSON object field.
func DeleteField(data []byte, key string) ([]byte, bool) {
	return jsonpatch.DeleteTopLevelField(data, key)
}

// FlattenArray returns the top-level elements of a JSON array as raw byte
// slices. The returned RawMessages alias data.
func FlattenArray(data []byte) ([]json.RawMessage, bool) {
	return jsonpatch.FlattenArray(data)
}

// ObjectField is re-exported from jsonpatch — a single key/value pair
// for use with FlattenObject / MarshalObjectFields.
type ObjectField = jsonpatch.ObjectField

// FlattenObject iterates a JSON object's key/value pairs in source order.
func FlattenObject(data []byte) ([]ObjectField, bool) {
	return jsonpatch.FlattenObject(data)
}

// MarshalObjectFields emits a JSON object preserving the given field order.
func MarshalObjectFields(fields []ObjectField) []byte {
	return jsonpatch.MarshalObjectFields(fields)
}

// LooksLikeString reports whether data is a JSON-encoded string after
// leading JSON whitespace.
func LooksLikeString(data []byte) bool {
	return jsonpatch.LooksLikeString(data)
}

// TrimWS returns data with leading/trailing JSON whitespace removed.
func TrimWS(data []byte) []byte {
	return jsonpatch.TrimWS(data)
}

// MarshalNoEscape is json.Marshal without HTML escaping of `<`, `>`, `&`.
// Use this for any bytes that land in an outbound LLM request body so the
// proxy's mutations don't flip literal angle brackets into `\uXXXX` and
// bust Anthropic's prompt cache.
func MarshalNoEscape(v any) ([]byte, error) {
	return jsonpatch.MarshalNoEscape(v)
}
