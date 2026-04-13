package executor

import (
	"bytes"
	"encoding/base64"
	"unicode/utf8"
)

// OutputResult holds captured output from a process stream.
type OutputResult struct {
	Data      string `json:"data"`
	Encoding  string `json:"encoding,omitempty"` // "" for utf-8, "base64" for binary
	Truncated bool   `json:"truncated,omitempty"`
}

// ProcessOutput processes raw captured bytes, applying size limits, binary detection,
// and UTF-8 validation.
func ProcessOutput(raw []byte, maxSize int64) OutputResult {
	truncated := false
	if int64(len(raw)) > maxSize {
		raw = raw[:maxSize]
		truncated = true
	}

	if isBinary(raw) {
		return OutputResult{
			Data:      base64.StdEncoding.EncodeToString(raw),
			Encoding:  "base64",
			Truncated: truncated,
		}
	}

	// Validate UTF-8, replacing invalid bytes.
	if !utf8.Valid(raw) {
		raw = replaceInvalidUTF8(raw)
	}

	return OutputResult{
		Data:      string(raw),
		Truncated: truncated,
	}
}

// isBinary checks if data contains null bytes in the first 512 bytes.
func isBinary(data []byte) bool {
	check := data
	if len(check) > 512 {
		check = check[:512]
	}
	return bytes.ContainsRune(check, 0)
}

// replaceInvalidUTF8 replaces invalid UTF-8 bytes with U+FFFD.
func replaceInvalidUTF8(data []byte) []byte {
	var buf bytes.Buffer
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			buf.WriteRune('\uFFFD')
		} else {
			buf.WriteRune(r)
		}
		data = data[size:]
	}
	return buf.Bytes()
}
