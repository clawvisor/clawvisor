// Package jsonpatch provides byte-faithful surgical edits on JSON
// byte slices. Lives as a leaf package — no dependencies beyond the
// standard library — so it can be imported from both llmproxy and
// conversation/stream without forming a cycle.
//
// The implementation mirrors llmproxy's json_surgery.go for the
// operations conversation/stream needs (single top-level field
// replacement / append). Keep the two in lockstep when changing
// behavior; llmproxy.SetJSONField is a thin wrapper around
// SetTopLevelField.
package jsonpatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

var errNotObject = errors.New("JSON value is not an object")

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
	if start, end, ok := findFieldValue(data, key); ok {
		return slices.Concat(data[:start], newValue, data[end:]), nil
	}
	return appendField(data, key, newValue)
}

func findFieldValue(data []byte, key string) (int, int, bool) {
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
		keyEnd := int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false
		}
		valueEnd := int(dec.InputOffset())
		if k == key {
			// Decoder's InputOffset after reading the key sits just
			// past the closing quote of the key — BEFORE the `:`. Scan
			// forward past `:` and whitespace to find the actual value
			// start so substitution doesn't clobber the colon.
			p := keyEnd
			for p < len(data) && data[p] != ':' {
				p++
			}
			if p >= len(data) {
				return 0, 0, false
			}
			p++ // past ':'
			for p < len(data) && isWhitespace(data[p]) {
				p++
			}
			return p, valueEnd, true
		}
	}
	return 0, 0, false
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
