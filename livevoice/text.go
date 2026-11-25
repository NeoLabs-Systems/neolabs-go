package livevoice

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// jsSpace is the class JavaScript's \s matches; RE2's own \s is ASCII only.
const jsSpace = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	trailingSpace  = regexp.MustCompile(`[` + jsSpace + `]$`)
	leadingBreak   = regexp.MustCompile(`^[` + jsSpace + `.,!?;:)]`)
	sentenceBreaks = regexp.MustCompile(`[.!?][` + jsSpace + `]+`)
)

// AppendFragment joins transcript fragments. They arrive with or without
// their leading space depending on the provider; words neither merge nor
// double-space.
func AppendFragment(buffer, fragment string) string {
	if buffer == "" {
		return strings.TrimLeftFunc(fragment, isJSSpace)
	}
	if fragment == "" {
		return buffer
	}
	if trailingSpace.MatchString(buffer) || leadingBreak.MatchString(fragment) {
		return buffer + fragment
	}
	return buffer + " " + fragment
}

// isJSSpace is JavaScript's whitespace set, which String.prototype.trim and
// the \s class share.
func isJSSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r == ' ', r == 0xa0, r == 0x1680, r >= 0x2000 && r <= 0x200a,
		r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000, r == 0xfeff:
		return true
	}
	return false
}

// TrimJS trims like JavaScript's String.prototype.trim.
func TrimJS(s string) string { return strings.TrimFunc(s, isJSSpace) }

// SplitForAppend cuts a long text into consecutive sentence-aligned parts of at
// most maxUnits UTF-16 code units. Live providers cap each injected context
// message (GPT-Live: 500 tokens), so a long task result is sent in parts.
func SplitForAppend(text string, maxUnits int) []string {
	value := TrimJS(text)
	if value == "" {
		return nil
	}
	if utf16Len(value) <= maxUnits {
		return []string{value}
	}
	var parts []string
	current := ""
	for _, sentence := range splitSentences(value) {
		if current != "" && utf16Len(current)+utf16Len(sentence)+1 > maxUnits {
			parts = append(parts, current)
			current = ""
		}
		if utf16Len(sentence) > maxUnits {
			units := utf16.Encode([]rune(sentence))
			for i := 0; i < len(units); i += maxUnits {
				end := min(i+maxUnits, len(units))
				parts = append(parts, string(utf16.Decode(units[i:end])))
			}
			continue
		}
		if current == "" {
			current = sentence
		} else {
			current = current + " " + sentence
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// splitSentences is value.split(/(?<=[.!?])\s+/): break after . ! or ?
// wherever whitespace follows, dropping the whitespace.
func splitSentences(value string) []string {
	var out []string
	last := 0
	for _, loc := range sentenceBreaks.FindAllStringIndex(value, -1) {
		// The punctuation stays with the sentence; the run of whitespace goes.
		out = append(out, value[last:loc[0]+1])
		last = loc[1]
	}
	return append(out, value[last:])
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }
