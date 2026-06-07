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

// LooksLikeString reports whether data is a JSON-encoded string after
// leading JSON whitespace.
func LooksLikeString(data []byte) bool {
	return jsonpatch.LooksLikeString(data)
}

// TrimWS returns data with leading/trailing JSON whitespace removed.
func TrimWS(data []byte) []byte {
	return jsonpatch.TrimWS(data)
}
