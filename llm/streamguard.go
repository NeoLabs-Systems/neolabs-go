package llm

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Limits of the output guards. MaxBytes is a safety ceiling on one streamed
// argument, far above what a model emits in a turn; the repetition check is what
// catches runaways. Sizes count UTF-16 units, like the Node implementation.
const (
	guardMaxBytes        = 2 * 1024 * 1024
	guardWindowBytes     = 1024
	guardCheckEveryBytes = 256
	guardMaxPeriod       = 64
)

// GuardVerdict says why a stream stopped being a real answer.
type GuardVerdict struct {
	Reason string // output_limit, degenerate_repetition, unknown_argument_key, invalid_json_arguments
	Bytes  int
	Period int
	Key    string
}

// StreamGuard watches one growing model output (a tool-call argument string or
// the reply text). It reports when the byte ceiling is gone or the latest window
// is a repeating whitespace/punctuation pattern, which is how small models fail
// when they lock into a loop and stream until max_tokens.
type StreamGuard struct {
	buffer         []uint16
	checkedLength  int
	maxBytes       int
	windowBytes    int
	checkEveryByte int
	maxPeriod      int
}

// NewStreamGuard returns a guard with the default limits.
func NewStreamGuard() *StreamGuard {
	return &StreamGuard{maxBytes: guardMaxBytes, windowBytes: guardWindowBytes, checkEveryByte: guardCheckEveryBytes, maxPeriod: guardMaxPeriod}
}

// Feed adds a delta and returns a verdict when the output must be stopped.
func (g *StreamGuard) Feed(delta string) *GuardVerdict {
	if delta == "" {
		return nil
	}
	g.buffer = append(g.buffer, utf16.Encode([]rune(delta))...)
	if len(g.buffer) > g.maxBytes {
		return &GuardVerdict{Reason: "output_limit", Bytes: len(g.buffer)}
	}
	if len(g.buffer) < g.windowBytes || len(g.buffer)-g.checkedLength < g.checkEveryByte {
		return nil
	}
	g.checkedLength = len(g.buffer)
	if period := repeatingPeriod(g.buffer[len(g.buffer)-g.windowBytes:], g.maxPeriod); period > 0 {
		return &GuardVerdict{Reason: "degenerate_repetition", Bytes: len(g.buffer), Period: period}
	}
	return nil
}

// repeatingPeriod: a window is degenerate when it is one short unit repeated end
// to end and that unit carries no letters or digits. Real data can be periodic
// too (a zero-filled array, a rule of dashes), so anything alphanumeric is left
// alone.
func repeatingPeriod(text []uint16, maxPeriod int) int {
	for period := 1; period <= maxPeriod; period++ {
		repeats := true
		for i := 0; i+period < len(text); i++ {
			if text[i] != text[i+period] {
				repeats = false
				break
			}
		}
		if !repeats {
			continue
		}
		for _, u := range text[:period] {
			if u >= '0' && u <= '9' || u >= 'A' && u <= 'Z' || u >= 'a' && u <= 'z' {
				return 0
			}
		}
		return period
	}
	return 0
}

// JSONPrefixTracker is an incremental check on a streamed tool-call argument
// string: it must remain a prefix of valid JSON and every top-level key must be
// one the tool declares. Runaway argument streams go wrong within the first few
// bytes and are usually still well-formed JSON, so the key check ends them
// immediately instead of after a kilobyte of repetition.
type JSONPrefixTracker struct {
	isKnownKey  func(string) bool
	stack       []byte // 'o' object, 'a' array
	expect      string
	inString    bool
	stringIsKey bool
	keyText     strings.Builder
	escape      bool
	unicodeLeft int
	literal     string
	valid       bool
	reason      string
	rejectedKey string
}

// NewJSONPrefixTracker returns a tracker. isKnownKey may be nil (all keys fine).
func NewJSONPrefixTracker(isKnownKey func(string) bool) *JSONPrefixTracker {
	if isKnownKey == nil {
		isKnownKey = func(string) bool { return true }
	}
	return &JSONPrefixTracker{isKnownKey: isKnownKey, expect: "value", valid: true}
}

// Reason is why the stream was rejected ("" while it is fine).
func (t *JSONPrefixTracker) Reason() string { return t.reason }

// Key is the undeclared key that ended the stream, if one did.
func (t *JSONPrefixTracker) Key() string { return t.rejectedKey }

// Feed adds argument text; false means the stream is no longer valid.
func (t *JSONPrefixTracker) Feed(delta string) bool {
	if !t.valid {
		return false
	}
	for _, ch := range delta {
		if !t.feedChar(ch) {
			t.valid = false
			if t.reason == "" {
				t.reason = "invalid_json_arguments"
			}
			return false
		}
	}
	return true
}

func (t *JSONPrefixTracker) afterValue() {
	if len(t.stack) > 0 {
		t.expect = "sep"
	} else {
		t.expect = "done"
	}
}

func (t *JSONPrefixTracker) closeString() bool {
	t.inString = false
	if !t.stringIsKey {
		t.afterValue()
		return true
	}
	t.expect = "colon"
	if len(t.stack) == 1 && !t.isKnownKey(t.keyText.String()) {
		t.reason = "unknown_argument_key"
		t.rejectedKey = t.keyText.String()
		return false
	}
	return true
}

func isNumberStart(literal string) bool {
	return literal != "" && (literal[0] == '-' || literal[0] >= '0' && literal[0] <= '9')
}

func (t *JSONPrefixTracker) finishLiteral() bool {
	var ok bool
	if isNumberStart(t.literal) {
		ok = numberFull(t.literal)
	} else {
		ok = t.literal == "true" || t.literal == "false" || t.literal == "null"
	}
	t.literal = ""
	if ok {
		t.afterValue()
	}
	return ok
}

// jsSpace is JavaScript's \s.
func jsSpace(ch rune) bool {
	switch ch {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return ch >= 0x2000 && ch <= 0x200a
}

func isHex(ch rune) bool {
	return ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F'
}

func (t *JSONPrefixTracker) feedChar(ch rune) bool {
	if t.inString {
		if t.stringIsKey && ch != '"' {
			t.keyText.WriteRune(ch)
		}
		if t.unicodeLeft > 0 {
			if !isHex(ch) {
				return false
			}
			t.unicodeLeft--
			return true
		}
		if t.escape {
			t.escape = false
			if ch == 'u' {
				t.unicodeLeft = 4
				return true
			}
			return strings.ContainsRune(`"\/bfnrt`, ch)
		}
		if ch == '\\' {
			t.escape = true
			return true
		}
		if ch == '"' {
			return t.closeString()
		}
		return true
	}
	if t.literal != "" {
		next := t.literal + string(ch)
		var stillPrefix bool
		if isNumberStart(next) {
			stillPrefix = numberPrefix(next)
		} else {
			for _, word := range []string{"true", "false", "null"} {
				if strings.HasPrefix(word, next) {
					stillPrefix = true
					break
				}
			}
		}
		if stillPrefix {
			t.literal = next
			return true
		}
		if !t.finishLiteral() {
			return false
		}
		// fall through: ch terminates the literal and must be handled itself
	}
	if jsSpace(ch) {
		return true
	}
	if t.expect == "done" {
		return false
	}
	var top byte
	if len(t.stack) > 0 {
		top = t.stack[len(t.stack)-1]
	}
	// firstKey / firstValue right after an opener also allow the closer, so `{}`
	// and `[]` pass while `{"a":1,}` and `[1,]` do not.
	wantsValue := t.expect == "value" || t.expect == "firstValue"
	wantsKey := t.expect == "key" || t.expect == "firstKey"
	switch ch {
	case '{':
		if !wantsValue {
			return false
		}
		t.stack = append(t.stack, 'o')
		t.expect = "firstKey"
		return true
	case '[':
		if !wantsValue {
			return false
		}
		t.stack = append(t.stack, 'a')
		t.expect = "firstValue"
		return true
	case '"':
		if !wantsValue && !wantsKey {
			return false
		}
		t.inString, t.stringIsKey = true, wantsKey
		t.keyText.Reset()
		return true
	case ':':
		if t.expect != "colon" {
			return false
		}
		t.expect = "value"
		return true
	case ',':
		if t.expect != "sep" {
			return false
		}
		if top == 'o' {
			t.expect = "key"
		} else {
			t.expect = "value"
		}
		return true
	case '}':
		if top != 'o' || (t.expect != "sep" && t.expect != "firstKey") {
			return false
		}
		t.stack = t.stack[:len(t.stack)-1]
		t.afterValue()
		return true
	case ']':
		if top != 'a' || (t.expect != "sep" && t.expect != "firstValue") {
			return false
		}
		t.stack = t.stack[:len(t.stack)-1]
		t.afterValue()
		return true
	default:
		if !wantsValue || !strings.ContainsRune("-0123456789tfn", ch) {
			return false
		}
		t.literal = string(ch)
		return true
	}
}

// numberPrefix matches /^-?(0|[1-9]\d*)?(\.\d*)?([eE][+-]?\d*)?$/.
func numberPrefix(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i < len(s) && s[i] == '0' {
		i++
	} else {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	return i == len(s)
}

// numberFull matches /^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?$/.
func numberFull(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}

// DegenerateOutputError is returned when a stream is stopped by the guards.
type DegenerateOutputError struct {
	Provider    string
	Reason      string
	OutputBytes int
	ToolName    string
	ArgumentKey string
}

func (e *DegenerateOutputError) Error() string {
	return fmt.Sprintf("%s stream aborted: %s after %d bytes of output.", e.Provider, e.Reason, e.OutputBytes)
}

// NewDegenerateOutputError builds the error for a guard verdict.
func NewDegenerateOutputError(provider string, v GuardVerdict, toolName string) *DegenerateOutputError {
	return &DegenerateOutputError{Provider: provider, Reason: v.Reason, OutputBytes: v.Bytes, ToolName: toolName, ArgumentKey: v.Key}
}

// AsDegenerateOutput finds the verdict in err or in what it wraps.
func AsDegenerateOutput(err error) (*DegenerateOutputError, bool) {
	var target *DegenerateOutputError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
