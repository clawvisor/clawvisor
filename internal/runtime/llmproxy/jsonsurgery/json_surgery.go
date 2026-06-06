package jsonsurgery

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
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

// FindFieldValue locates the byte range of the value associated
// with the given top-level key in a JSON object. Returns
// (valueStart, valueEnd, true) such that data[valueStart:valueEnd] is
// the raw JSON value, or (_, _, false) if data isn't a JSON object or
// the key isn't present.
func FindFieldValue(data []byte, key string) (int, int, bool) {
	_, valueStart, valueEnd, ok := findJSONFieldSpan(data, key)
	return valueStart, valueEnd, ok
}

// findJSONFieldSpan locates byte ranges for both the key string and
// the value of a top-level field. Returns (keyOpenQuote, valueStart,
// valueEnd, true) on success where data[keyOpenQuote] == '"' is the
// opening quote of the JSON-encoded key. Used by DeleteField so
// it can locate the start of the key without a backward scan
// (which is brittle when the key contains escaped quotes).
func findJSONFieldSpan(data []byte, key string) (int, int, int, bool) {
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
		keyEnd := int(dec.InputOffset()) // just past closing quote of key
		if k != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, 0, false
			}
			continue
		}
		// Walk backward from the closing quote to find the matching
		// opening quote, accounting for escaped quotes. `\"` inside
		// the key string must not terminate the backward scan.
		closeQuote := keyEnd - 1 // position of `"` that closes the key
		openQuote := closeQuote - 1
		for openQuote >= 0 {
			if data[openQuote] != '"' {
				openQuote--
				continue
			}
			// Count consecutive backslashes immediately before this
			// quote; an odd count means it's escaped.
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
		// Scan forward past ':' and whitespace to find value start.
		p := keyEnd
		for p < len(data) && data[p] != ':' {
			p++
		}
		if p >= len(data) {
			return 0, 0, 0, false
		}
		p++ // past ':'
		for p < len(data) && isJSONWS(data[p]) {
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

// SetField returns data with the value at the given top-level key
// replaced by newValue. If the key doesn't exist, it's appended just
// before the closing `}`. All unmodified bytes — including key order,
// whitespace, and any fields we don't model — are preserved verbatim.
//
// newValue must be valid JSON (object, array, string, number, bool,
// or null). Callers typically construct it via json.Marshal of a
// single value, never via json.Marshal of an envelope.
func SetField(data []byte, key string, newValue []byte) ([]byte, error) {
	if start, end, ok := FindFieldValue(data, key); ok {
		return slices.Concat(data[:start], newValue, data[end:]), nil
	}
	return appendJSONField(data, key, newValue)
}

// DeleteField returns data with the named top-level field
// removed. If the key isn't present, data is returned unchanged.
func DeleteField(data []byte, key string) ([]byte, bool) {
	keyOpenQuote, _, valueEnd, ok := findJSONFieldSpan(data, key)
	if !ok {
		return data, false
	}
	removeStart := keyOpenQuote
	removeEnd := valueEnd
	// Eat surrounding whitespace, then decide whether to consume a
	// leading or trailing comma so the resulting object stays valid.
	for removeStart > 0 && isJSONWS(data[removeStart-1]) {
		removeStart--
	}
	for removeEnd < len(data) && isJSONWS(data[removeEnd]) {
		removeEnd++
	}
	if removeStart > 0 && data[removeStart-1] == ',' {
		removeStart--
	} else if removeEnd < len(data) && data[removeEnd] == ',' {
		removeEnd++
	}
	return slices.Concat(data[:removeStart], data[removeEnd:]), true
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
	var insert []byte
	if hasExistingFields {
		insert = append(insert, ',')
	}
	insert = append(insert, encodedKey...)
	insert = append(insert, ':')
	insert = append(insert, newValue...)
	return slices.Concat(data[:closeBracePos], insert, data[closeBracePos:]), nil
}

func isJSONWS(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

var jsonNotObjectErr = errors.New("JSON value is not an object")

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

// FlattenArray returns the top-level elements of a JSON array as
// raw byte slices, preserving each element's bytes verbatim. The
// returned slices alias into data. Returns nil, false if data isn't a
// JSON array.
func FlattenArray(data []byte) ([]json.RawMessage, bool) {
	if !jsonObjectIsArray(data) {
		return nil, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return nil, false
	}
	return elems, true
}

// LooksLikeString reports whether data is a JSON-encoded string
// (leading quote, after whitespace). Useful for distinguishing
// string-content from array-content.
func LooksLikeString(data []byte) bool {
	for _, b := range data {
		if isJSONWS(b) {
			continue
		}
		return b == '"'
	}
	return false
}

// TrimWS returns data with leading/trailing JSON whitespace
// removed.
func TrimWS(data []byte) []byte {
	return bytes.TrimFunc(data, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
