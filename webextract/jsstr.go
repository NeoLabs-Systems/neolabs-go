package webextract

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// The extractor follows JavaScript string rules: \s covers Unicode spaces,
// trim() strips the same set and lengths count UTF-16 code units.

const jsSpaceClass = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	jsSpaceRun = regexp.MustCompile(`[` + jsSpaceClass + `]+`)
)

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// collapse is s.replace(/\s+/g, ' ').
func collapse(s string) string { return jsSpaceRun.ReplaceAllString(s, " ") }

// utf16Len is a JavaScript string's length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Prefix is s.slice(0, n).
func utf16Prefix(s string, n int) string {
	if utf16Len(s) <= n {
		return s
	}
	units := utf16.Encode([]rune(s))[:n]
	return string(utf16.Decode(units))
}

// wordCount is text.replace(/\s+/g, ' ').trim().split(' ').filter(Boolean).length.
func wordCount(text string) int {
	n := 0
	for _, w := range strings.Split(jsTrim(collapse(text)), " ") {
		if w != "" {
			n++
		}
	}
	return n
}
