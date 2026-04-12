package services

import (
	"fmt"
	"strings"
)

// Shlex splits a string into tokens using POSIX shell tokenization rules.
// It supports single quotes, double quotes with backslash escapes, and
// backslash escaping outside quotes. It does NOT invoke a shell.
func Shlex(s string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inToken := false

	i := 0
	for i < len(s) {
		ch := s[i]

		switch {
		case ch == '\'':
			// Single-quoted string: everything until closing single quote is literal.
			inToken = true
			i++
			for i < len(s) && s[i] != '\'' {
				current.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i++ // skip closing quote

		case ch == '"':
			// Double-quoted string: backslash escapes work inside.
			inToken = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					current.WriteByte(s[i])
				} else {
					current.WriteByte(s[i])
				}
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i++ // skip closing quote

		case ch == '\\':
			// Backslash outside quotes: escape next character.
			inToken = true
			i++
			if i >= len(s) {
				return nil, fmt.Errorf("trailing backslash")
			}
			current.WriteByte(s[i])
			i++

		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			// Whitespace: end current token.
			if inToken {
				tokens = append(tokens, current.String())
				current.Reset()
				inToken = false
			}
			i++

		default:
			inToken = true
			current.WriteByte(ch)
			i++
		}
	}

	if inToken {
		tokens = append(tokens, current.String())
	}

	return tokens, nil
}
