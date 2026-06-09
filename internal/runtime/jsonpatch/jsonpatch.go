// Package jsonpatch provides byte-faithful surgical edits on JSON
// byte slices. It lives as a leaf package so llmproxy and
// conversation/stream share one implementation without forming cycles.
package jsonpatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

var errNotObject = errors.New("JSON value is not an object")

// FindTopLevelFieldValue locates the byte range of the value associated
// with key in a top-level JSON object. The returned range indexes data.
func FindTopLevelFieldValue(data []byte, key string) (int, int, bool) {
	_, valueStart, valueEnd, ok := findTopLevelFieldSpan(data, key)
	return valueStart, valueEnd, ok
}

// SetTopLevelField returns data with the value at the given top-level
// key replaced by newValue. If the key doesn't exist, it's appended
// just before the closing `}`. All unmodified bytes — including key
// order, whitespace, and any fields we don't model — are preserved
// verbatim.
//
// newValue must be valid JSON (object, array, string, number, bool,
// or null). Callers typically construct it via json.Marshal of a
// single value, never via json.Marshal of an envelope.
func SetTopLevelField(data []byte, key string, newValue []byte) ([]byte, error) {
	if !json.Valid(newValue) {
		return nil, errors.New("newValue is not valid JSON")
	}
	if start, end, ok := FindTopLevelFieldValue(data, key); ok {
		return slices.Concat(data[:start], newValue, data[end:]), nil
	}
	return appendField(data, key, newValue)
}

// DeleteTopLevelField returns data with the named top-level field removed.
// If key is absent, data is returned unchanged and ok is false.
func DeleteTopLevelField(data []byte, key string) ([]byte, bool) {
	keyOpenQuote, _, valueEnd, ok := findTopLevelFieldSpan(data, key)
	if !ok {
		return data, false
	}
	removeStart := keyOpenQuote
	removeEnd := valueEnd
	for removeStart > 0 && isWhitespace(data[removeStart-1]) {
		removeStart--
	}
	for removeEnd < len(data) && isWhitespace(data[removeEnd]) {
		removeEnd++
	}
	if removeStart > 0 && data[removeStart-1] == ',' {
		removeStart--
	} else if removeEnd < len(data) && data[removeEnd] == ',' {
		removeEnd++
	}
	return slices.Concat(data[:removeStart], data[removeEnd:]), true
}

// FlattenArray returns the top-level elements of a JSON array as raw byte
// slices. The returned RawMessages alias data.
func FlattenArray(data []byte) ([]json.RawMessage, bool) {
	if !looksLikeArray(data) {
		return nil, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return nil, false
	}
	return elems, true
}

// ObjectField is a single key/value pair in a JSON object. When
// produced by FlattenObject, Value is a RawMessage over the original
// input bytes so unchanged fields can be reassembled verbatim by
// MarshalObjectFields.
type ObjectField struct {
	Key   string
	Value json.RawMessage
}

// FlattenObject iterates a JSON object's key/value pairs in source
// order. Values come back as RawMessages over the input bytes
// (including internal whitespace), letting callers do surgical edits
// without canonicalizing key order — important for Anthropic thinking
// blocks where any byte change invalidates the signature.
//
// Returns ok=false for any input that is not exactly one well-formed
// JSON object: truncated input (`{"a":1`), trailing garbage
// (`{"a":1}xyz`), concatenated objects (`{"a":1}{"b":2}`), and so on.
// This mirrors the strictness of FlattenArray (which uses json.Unmarshal)
// so rewrite paths never operate on partial payloads.
func FlattenObject(data []byte) ([]ObjectField, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var fields []ObjectField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		fields = append(fields, ObjectField{Key: key, Value: val})
	}
	// dec.More() returning false on a truncated object (no closing
	// brace) doesn't itself signal error — the decoder just stops. We
	// must explicitly consume the closing '}' and then confirm the
	// stream is exhausted, so a missing '}' or trailing tokens both
	// fail the parse instead of silently stripping bytes.
	closeTok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := closeTok.(json.Delim); !ok || d != '}' {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// MarshalObjectFields emits a compact JSON object preserving the given
// field order. Values are written verbatim. The dual of FlattenObject:
// FlattenObject → mutate values → MarshalObjectFields round-trips an
// object without reordering its keys.
func MarshalObjectFields(fields []ObjectField) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyEnc, _ := json.Marshal(f.Key)
		buf.Write(keyEnc)
		buf.WriteByte(':')
		buf.Write(f.Value)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// LooksLikeString reports whether data begins with a JSON string after
// leading JSON whitespace.
func LooksLikeString(data []byte) bool {
	for _, b := range data {
		if isWhitespace(b) {
			continue
		}
		return b == '"'
	}
	return false
}

// TrimWS returns data with leading/trailing JSON whitespace removed.
func TrimWS(data []byte) []byte {
	return bytes.TrimFunc(data, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func findTopLevelFieldSpan(data []byte, key string) (int, int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, 0, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, 0, false
		}
		k, ok := keyTok.(string)
		if !ok {
			return 0, 0, 0, false
		}
		keyEnd := int(dec.InputOffset())
		if k != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, 0, false
			}
			continue
		}

		closeQuote := keyEnd - 1
		openQuote := closeQuote - 1
		for openQuote >= 0 {
			if data[openQuote] != '"' {
				openQuote--
				continue
			}
			bs := 0
			for q := openQuote - 1; q >= 0 && data[q] == '\\'; q-- {
				bs++
			}
			if bs%2 == 0 {
				break
			}
			openQuote--
		}
		if openQuote < 0 {
			return 0, 0, 0, false
		}

		p := keyEnd
		for p < len(data) && data[p] != ':' {
			p++
		}
		if p >= len(data) {
			return 0, 0, 0, false
		}
		p++
		for p < len(data) && isWhitespace(data[p]) {
			p++
		}
		valueStart := p
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return 0, 0, 0, false
		}
		return openQuote, valueStart, int(dec.InputOffset()), true
	}
	return 0, 0, 0, false
}

func appendField(data []byte, key string, newValue []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	hasExistingFields := dec.More()
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	closeBracePos := int(dec.InputOffset()) - 1
	for closeBracePos > 0 && isWhitespace(data[closeBracePos-1]) {
		closeBracePos--
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	var insert []byte
	if hasExistingFields {
		insert = append(insert, ',')
	}
	insert = append(insert, encodedKey...)
	insert = append(insert, ':')
	insert = append(insert, newValue...)
	return slices.Concat(data[:closeBracePos], insert, data[closeBracePos:]), nil
}

func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func looksLikeArray(data []byte) bool {
	for _, b := range data {
		if isWhitespace(b) {
			continue
		}
		return b == '['
	}
	return false
}
