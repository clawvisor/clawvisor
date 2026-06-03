package llmproxy

import (
	"bytes"
	"encoding/json"
)

// Byte-faithful surgical edits on JSON byte slices.
//
// The Anthropic streaming protocol enforces a cryptographic signature
// over thinking blocks across turns. Any reshape — reordered keys,
// stripped whitespace, applied `,omitempty`, dropped unknown fields —
// risks "thinking blocks cannot be modified" 400s on subsequent
// requests. The conventional `json.Unmarshal` →
// `map[string]json.RawMessage` → `json.Marshal` round-trip alphabetizes
// keys and drops unknown fields, so it cannot be used on bodies that
// carry assistant turns we need to pass back upstream verbatim.
//
// These helpers operate on the raw byte representation. They use
// `encoding/json.Decoder` to navigate JSON structure (so nested keys
// don't collide with top-level ones), then splice replacements into
// the original byte slice without re-emitting surrounding context.

// findJSONFieldValue locates the byte range of the value associated
// with the given top-level key in a JSON object. Returns
// (valueStart, valueEnd, true) such that data[valueStart:valueEnd] is
// the raw JSON value, or (_, _, false) if data isn't a JSON object or
// the key isn't present.
func findJSONFieldValue(data []byte, key string) (int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		k, ok := keyTok.(string)
		if !ok {
			return 0, 0, false
		}
		if k != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, false
			}
			continue
		}
		// dec.InputOffset() sits just past the closing quote of the
		// matched key. Scan forward past ':' and whitespace to find
		// where the value begins.
		p := int(dec.InputOffset())
		for p < len(data) && data[p] != ':' {
			p++
		}
		if p >= len(data) {
			return 0, 0, false
		}
		p++ // past ':'
		for p < len(data) && isJSONWS(data[p]) {
			p++
		}
		valueStart := p
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return 0, 0, false
		}
		return valueStart, int(dec.InputOffset()), true
	}
	return 0, 0, false
}

// SetJSONField returns data with the value at the given top-level key
// replaced by newValue. If the key doesn't exist, it's appended just
// before the closing `}`. All unmodified bytes — including key order,
// whitespace, and any fields we don't model — are preserved verbatim.
//
// newValue must be valid JSON (object, array, string, number, bool,
// or null). Callers typically construct it via json.Marshal of a
// single value, never via json.Marshal of an envelope.
func SetJSONField(data []byte, key string, newValue []byte) ([]byte, error) {
	if start, end, ok := findJSONFieldValue(data, key); ok {
		out := make([]byte, 0, len(data)+len(newValue)-(end-start))
		out = append(out, data[:start]...)
		out = append(out, newValue...)
		out = append(out, data[end:]...)
		return out, nil
	}
	return appendJSONField(data, key, newValue)
}

// DeleteJSONField returns data with the named top-level field
// removed. If the key isn't present, data is returned unchanged.
func DeleteJSONField(data []byte, key string) ([]byte, bool) {
	valueStart, valueEnd, ok := findJSONFieldValue(data, key)
	if !ok {
		return data, false
	}
	// Walk backward from valueStart past the colon and the key string
	// to find the start of `"<key>"`.
	keyStart := valueStart
	for keyStart > 0 && data[keyStart-1] != '"' {
		keyStart--
	}
	if keyStart <= 0 {
		return data, false
	}
	// Now keyStart points one past a closing quote of the key. Walk
	// past `:` and whitespace backward.
	p := keyStart - 1 // closing quote
	// Walk back across the key string itself.
	for p > 0 && data[p-1] != '"' {
		p--
	}
	if p == 0 {
		return data, false
	}
	keyOpenQuote := p - 1 // position of opening quote of key
	// Now decide what to remove on either side. We want to remove
	// either a leading comma (if this isn't the first field) or a
	// trailing comma (if it is). Skip whitespace.
	removeStart := keyOpenQuote
	removeEnd := valueEnd
	// Skip whitespace before the key opening quote.
	for removeStart > 0 && isJSONWS(data[removeStart-1]) {
		removeStart--
	}
	// Skip whitespace after the value.
	for removeEnd < len(data) && isJSONWS(data[removeEnd]) {
		removeEnd++
	}
	// Now: if data[removeStart-1] == ',' → drop leading comma.
	// Else if data[removeEnd] == ',' → drop trailing comma.
	// Else this was the only field (don't drop anything else).
	if removeStart > 0 && data[removeStart-1] == ',' {
		removeStart--
	} else if removeEnd < len(data) && data[removeEnd] == ',' {
		removeEnd++
	}
	out := make([]byte, 0, len(data)-(removeEnd-removeStart))
	out = append(out, data[:removeStart]...)
	out = append(out, data[removeEnd:]...)
	return out, true
}

// appendJSONField inserts `"key":<newValue>` just before the closing
// brace of a JSON object, adding a leading comma if there are existing
// fields. Returns an error only on malformed input.
func appendJSONField(data []byte, key string, newValue []byte) ([]byte, error) {
	// Find the closing `}` of the top-level object. We need to walk
	// through the JSON to find the matching brace at depth 0.
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, jsonNotObjectErr
	}
	hasExistingFields := dec.More()
	// Drain to the closing brace.
	for dec.More() {
		// Read and discard key.
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	// Read the closing brace token to advance offset.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	closeBracePos := int(dec.InputOffset()) - 1
	// Walk back across whitespace.
	for closeBracePos > 0 && isJSONWS(data[closeBracePos-1]) {
		closeBracePos--
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	insert := make([]byte, 0, len(encodedKey)+1+len(newValue)+1)
	if hasExistingFields {
		insert = append(insert, ',')
	}
	insert = append(insert, encodedKey...)
	insert = append(insert, ':')
	insert = append(insert, newValue...)
	out := make([]byte, 0, len(data)+len(insert))
	out = append(out, data[:closeBracePos]...)
	out = append(out, insert...)
	out = append(out, data[closeBracePos:]...)
	return out, nil
}

func isJSONWS(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

var jsonNotObjectErr = &jsonSurgeryError{msg: "JSON value is not an object"}

type jsonSurgeryError struct {
	msg string
}

func (e *jsonSurgeryError) Error() string { return e.msg }

// jsonObjectIsArray reports whether the leading non-whitespace
// character of data is `[`.
func jsonObjectIsArray(data []byte) bool {
	for _, b := range data {
		if isJSONWS(b) {
			continue
		}
		return b == '['
	}
	return false
}

// flattenJSONArray returns the top-level elements of a JSON array as
// raw byte slices, preserving each element's bytes verbatim. The
// returned slices alias into data. Returns nil, false if data isn't a
// JSON array.
func flattenJSONArray(data []byte) ([]json.RawMessage, bool) {
	if !jsonObjectIsArray(data) {
		return nil, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return nil, false
	}
	return elems, true
}

// looksLikeJSONString reports whether data is a JSON-encoded string
// (leading quote, after whitespace). Useful for distinguishing
// string-content from array-content.
func looksLikeJSONString(data []byte) bool {
	for _, b := range data {
		if isJSONWS(b) {
			continue
		}
		return b == '"'
	}
	return false
}

// trimJSONWS returns data with leading/trailing JSON whitespace
// removed.
func trimJSONWS(data []byte) []byte {
	return bytes.TrimFunc(data, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
