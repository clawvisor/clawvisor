package autovault

import (
	"math"
	"regexp"
	"strings"
)

type Candidate struct {
	Value   string
	Start   int
	End     int
	Charset string
	Entropy float64
}

var (
	prefixBodyRe = regexp.MustCompile(`\b[A-Za-z]{2,16}[_-][A-Za-z0-9_-]{16,}\b`)
	blobRe       = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)
	uuidRe       = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	passwordRe   = regexp.MustCompile(`(?i)(?:password|passcode|secret|api[_ -]?key|access[_ -]?token)\s*(?:is|=|:)?\s*([A-Za-z0-9_./+=:-]{8,})`)
)

const minEntropyBits = 3.5

var englishTrigrams = map[string]bool{
	"the": true, "and": true, "ing": true, "her": true,
	"ere": true, "ent": true, "for": true, "tha": true,
	"nth": true, "int": true, "all": true, "ion": true,
	"ter": true, "est": true, "ers": true, "ati": true,
	"hat": true, "ate": true, "ver": true, "ith": true,
	"tio": true, "his": true, "per": true, "our": true,
}

func DetectCandidates(s string) []Candidate {
	if len(s) < 16 {
		return nil
	}
	seen := map[string]bool{}
	var out []Candidate
	add := func(match string, start, end int) {
		if match == "" || seen[match] || LooksLikeShadow(match) || looksLikeIdentifier(match) {
			return
		}
		ent := shannonEntropy(match)
		if ent < minEntropyBits || looksLikeEnglish(match) {
			return
		}
		seen[match] = true
		out = append(out, Candidate{
			Value:   match,
			Start:   start,
			End:     end,
			Charset: classifyCharset(match),
			Entropy: ent,
		})
	}
	for _, loc := range prefixBodyRe.FindAllStringIndex(s, -1) {
		add(s[loc[0]:loc[1]], loc[0], loc[1])
	}
	for _, loc := range blobRe.FindAllStringIndex(s, -1) {
		add(s[loc[0]:loc[1]], loc[0], loc[1])
	}
	for _, loc := range uuidRe.FindAllStringIndex(s, -1) {
		add(s[loc[0]:loc[1]], loc[0], loc[1])
	}
	return out
}

func looksLikeIdentifier(s string) bool {
	if len(s) < 16 {
		return false
	}
	hasSep := strings.ContainsAny(s, "_-")
	if !hasSep {
		return false
	}
	hasDigit := false
	hasUpper := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	if hasDigit || hasUpper {
		return false
	}
	return true
}

func FindPasswordRevealCandidates(s string) []string {
	matches := passwordRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		value := strings.TrimSpace(match[1])
		if value == "" || LooksLikeShadow(value) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[byte]int, 64)
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, count := range freq {
		p := float64(count) / n
		h -= p * math.Log2(p)
	}
	return h
}

func looksLikeEnglish(s string) bool {
	flat := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			flat = append(flat, c)
		case c >= 'A' && c <= 'Z':
			flat = append(flat, c+32)
		}
	}
	if len(flat) < 6 {
		return false
	}
	hits := 0
	for i := 0; i+3 <= len(flat); i++ {
		if englishTrigrams[string(flat[i:i+3])] {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

func classifyCharset(match string) string {
	hex := true
	alnum := true
	b64 := true
	for i := 0; i < len(match); i++ {
		c := match[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			hex = false
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			alnum = false
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '+' || c == '/' || c == '=' || c == '_' || c == '-':
		default:
			b64 = false
		}
	}
	switch {
	case hex:
		return "hex"
	case alnum:
		return "alnum"
	case strings.ContainsAny(match, "_-"):
		return "alnum+sep"
	case b64:
		return "base64"
	default:
		return "mixed"
	}
}
