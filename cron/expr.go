// Package cron schedules work from cron expressions with the semantics of
// node-cron 4: five or six fields (a leading seconds field is optional),
// nicknames such as @daily, month and weekday names, ranges that wrap around,
// the L, L-n, nW, LW, nL and n#m extensions, time zones, a tolerance for late
// ticks, overlap prevention and random start delay.
//
// One quirk is kept on purpose because stored schedules depend on it: the
// day-of-month and day-of-week fields must BOTH match (a field left as * matches
// every day), unlike Vixie cron, where either one may.
//
// The expression parser, the next-run search and the beat planner are checked
// against vectors generated from node-cron itself (see gen/gen.mjs).
package cron

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// token is one entry of a converted field: a number, or text such as "L",
// "15W" or "2#3" that only the day fields accept.
type token struct {
	num   int
	text  string
	isNum bool
}

func numToken(n int) token { return token{num: n, isNum: true} }

var (
	allowedChars   = regexp.MustCompile(`^[a-zA-Z0-9\-*/,#? ]+$`)
	multiSpace     = regexp.MustCompile(`\s{2,}`)
	rangeToken     = regexp.MustCompile(`^(\d+)-(\d+)(?:/(\d+))?$`)
	digitsOnly     = regexp.MustCompile(`^\d+$`)
	lastDayToken   = regexp.MustCompile(`(?i)^l$`)
	lastOffsetTok  = regexp.MustCompile(`(?i)^l-\d{1,2}$`)
	lastWeekdayTok = regexp.MustCompile(`(?i)^[0-7]l$`)
	hasW           = regexp.MustCompile(`(?i)w`)
	weekdayWToken  = regexp.MustCompile(`^(\d{1,2}|L)W$`)
	weekdayWTokeni = regexp.MustCompile(`(?i)^(\d{1,2}|L)W$`)
	offsetToken    = regexp.MustCompile(`^L-(\d{1,2})$`)
	offsetTokeni   = regexp.MustCompile(`(?i)^L-(\d{1,2})$`)
	lastWeekdayRe  = regexp.MustCompile(`(?i)^([0-7])L$`)
	nthWeekdayRe   = regexp.MustCompile(`^([0-7])#([1-5])$`)
	lastWeekdayStr = regexp.MustCompile(`^[0-7]L$`)
)

var nicknames = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

func resolveNickname(expression string) string {
	if n, ok := nicknames[strings.ToLower(strings.TrimSpace(expression))]; ok {
		return n
	}
	return expression
}

var (
	longMonths   = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	shortMonths  = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	longWeekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	shortWeekday = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

func replaceNames(expression string, names []string, first int) string {
	for i, name := range names {
		expression = regexp.MustCompile("(?i)"+name).ReplaceAllString(expression, strconv.Itoa(i+first))
	}
	return expression
}

// jsParseInt is Number.parseInt(s, 10): optional whitespace and sign, then as
// many digits as there are. Huge values saturate at 2^53 like a double would.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	const limit = 1 << 53
	n := 0
	for _, c := range s[:end] {
		n = n*10 + int(c-'0')
		if n > limit {
			n = limit
		}
	}
	if neg {
		n = -n
	}
	return n, true
}

type fieldBounds struct{ min, max int }

var fieldBoundsList = []fieldBounds{{0, 59}, {0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

var asteriskRanges = []string{"0-59", "0-59", "0-23", "1-31", "1-12", "0-6"}

func convertAsterisk(expression, replacement string) string {
	parts := strings.Split(expression, ",")
	for i, p := range parts {
		if strings.Contains(p, "*") {
			parts[i] = strings.Replace(p, "*", replacement, 1)
		}
	}
	return strings.Join(parts, ",")
}

// expandRange lists the numbers of "first-last/step". A range that runs past
// the field's bounds can never validate, so it is returned as its two ends
// instead of being expanded without limit.
func expandRange(initTxt, endTxt, stepTxt string, bounds fieldBounds) string {
	step, _ := jsParseInt(stepTxt)
	if !(step >= 1) {
		return initTxt + "-" + endTxt + "/" + stepTxt
	}
	first, _ := jsParseInt(initTxt)
	last, _ := jsParseInt(endTxt)
	if first <= last {
		if first < bounds.min || last > bounds.max {
			return strconv.Itoa(first) + "," + strconv.Itoa(last)
		}
		var nums []string
		for i := first; i <= last; i += step {
			nums = append(nums, strconv.Itoa(i))
		}
		return strings.Join(nums, ",")
	}
	size := bounds.max - bounds.min + 1
	span := ((last-first)%size + size) % size
	var nums []string
	for offset := 0; offset <= span; offset += step {
		value := first + offset
		if value > bounds.max {
			value -= size
		}
		nums = append(nums, strconv.Itoa(value))
	}
	return strings.Join(nums, ",")
}

func convertRange(expression string, bounds fieldBounds) string {
	parts := strings.Split(expression, ",")
	for i, p := range parts {
		if m := rangeToken.FindStringSubmatch(strings.TrimSpace(p)); m != nil {
			step := m[3]
			if step == "" {
				step = "1"
			}
			parts[i] = expandRange(m[1], m[2], step, bounds)
		}
	}
	return strings.Join(parts, ",")
}

func normalizeTokens(field string) []token {
	parts := strings.Split(field, ",")
	out := make([]token, len(parts))
	for i, raw := range parts {
		t := strings.TrimSpace(raw)
		switch {
		case lastDayToken.MatchString(t):
			out[i] = token{text: "L"}
		case lastOffsetTok.MatchString(t), lastWeekdayTok.MatchString(t), hasW.MatchString(t):
			out[i] = token{text: strings.ToUpper(t)}
		case strings.Contains(t, "#"):
			out[i] = token{text: t}
		case digitsOnly.MatchString(t):
			n, _ := jsParseInt(t)
			out[i] = numToken(n)
		default:
			out[i] = token{text: t}
		}
	}
	return out
}

// convertExpression turns an expression of five or six fields into six lists
// of tokens: second, minute, hour, day of month, month, day of week.
func convertExpression(expression string) [][]token {
	fields := strings.Split(strings.TrimSpace(multiSpace.ReplaceAllString(resolveNickname(expression), " ")), " ")
	if len(fields) == 5 {
		fields = append([]string{"0"}, fields...)
	}
	if len(fields) != 6 {
		return nil
	}
	if fields[3] == "?" {
		fields[3] = "*"
	}
	if fields[5] == "?" {
		fields[5] = "*"
	}
	fields[4] = replaceNames(replaceNames(fields[4], longMonths, 1), shortMonths, 1)
	fields[5] = replaceNames(replaceNames(fields[5], longWeekdays, 0), shortWeekday, 0)
	out := make([][]token, 6)
	for i := range fields {
		fields[i] = convertAsterisk(fields[i], asteriskRanges[i])
		fields[i] = convertRange(fields[i], fieldBoundsList[i])
		out[i] = normalizeTokens(fields[i])
	}
	seen := map[token]bool{}
	var weekdays []token
	for _, t := range out[5] {
		switch {
		case t.isNum && t.num == 7:
			t = numToken(0)
		case !t.isNum && strings.HasPrefix(t.text, "7"):
			t = token{text: "0" + t.text[1:]}
		}
		if !seen[t] {
			seen[t] = true
			weekdays = append(weekdays, t)
		}
	}
	out[5] = weekdays
	return out
}

func (t token) String() string {
	if t.isNum {
		return strconv.Itoa(t.num)
	}
	return t.text
}

var plainNumber = regexp.MustCompile(`^(?:\d+|\*|\*/\d+)$`)

// validTokens is isValidExpression: every token reads as a plain number within
// the field's bounds.
func validTokens(tokens []token, min, max int) bool {
	for _, t := range tokens {
		s := t.String()
		if n, ok := jsParseInt(s); ok && (n < min || n > max) {
			return false
		}
		if !plainNumber.MatchString(s) {
			return false
		}
	}
	return true
}

func invalidDayOfMonth(tokens []token) bool {
	var rest []token
	for _, t := range tokens {
		if !t.isNum {
			if t.text == "L" {
				continue
			}
			if m := weekdayWTokeni.FindStringSubmatch(t.text); m != nil {
				if m[1] == "L" {
					continue
				}
				target, _ := jsParseInt(m[1])
				if !(target < 1 || target > 31) {
					continue
				}
				rest = append(rest, t)
				continue
			}
			if m := offsetTokeni.FindStringSubmatch(t.text); m != nil {
				n, _ := jsParseInt(m[1])
				if !(n < 1 || n > 30) {
					continue
				}
			}
		}
		rest = append(rest, t)
	}
	return !validTokens(rest, 1, 31)
}

// invalidWModifier reports a W in the raw day-of-month text that is not a
// valid "nW" or "LW" token.
func invalidWModifier(raw string) bool {
	if !hasW.MatchString(raw) {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		v := strings.TrimSpace(part)
		if hasW.MatchString(v) && !weekdayWTokeni.MatchString(v) {
			return true
		}
	}
	return false
}

func isNthWeekdayToken(t token) bool { return !t.isNum && nthWeekdayRe.MatchString(t.text) }

func invalidWeekday(tokens []token) bool {
	var rest []token
	for _, t := range tokens {
		if isNthWeekdayToken(t) || (!t.isNum && lastWeekdayStr.MatchString(t.text)) {
			continue
		}
		rest = append(rest, t)
	}
	return !validTokens(rest, 0, 7)
}

var maxDaysInMonth = [...]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func impossibleDayOfMonth(days, months []token) bool {
	for _, d := range days {
		if !d.isNum {
			return false
		}
	}
	for _, m := range months {
		for _, d := range days {
			if m.isNum && m.num >= 1 && m.num <= 12 && d.num <= maxDaysInMonth[m.num] {
				return false
			}
		}
	}
	return true
}

type fieldSpec struct {
	label string
	check func(tokens []token) bool // true when invalid
}

var fieldSpecs = []fieldSpec{
	{"second", func(t []token) bool { return !validTokens(t, 0, 59) }},
	{"minute", func(t []token) bool { return !validTokens(t, 0, 59) }},
	{"hour", func(t []token) bool { return !validTokens(t, 0, 23) }},
	{"day of month", invalidDayOfMonth},
	{"month", func(t []token) bool { return !validTokens(t, 1, 12) }},
	{"week day", invalidWeekday},
}

// Error is an expression that is not valid; Message is node-cron's wording.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// convert validates expression and returns its converted fields.
func convert(expression string) ([][]token, error) {
	resolved := resolveNickname(expression)
	if !allowedChars.MatchString(resolved) {
		return nil, &Error{"pattern includes illegal characters"}
	}
	raw := strings.Split(strings.TrimSpace(multiSpace.ReplaceAllString(resolved, " ")), " ")
	if len(raw) != 5 && len(raw) != 6 {
		return nil, &Error{fmt.Sprintf("expected 5 or 6 fields but got %d", len(raw))}
	}
	patterns := raw
	if len(raw) == 5 {
		patterns = append([]string{"0"}, raw...)
	}
	fields := convertExpression(expression)
	for i, spec := range fieldSpecs {
		if spec.check(fields[i]) || (i == 3 && invalidWModifier(patterns[3])) {
			return nil, &Error{fmt.Sprintf("%s is a invalid expression for %s", patterns[i], spec.label)}
		}
	}
	if impossibleDayOfMonth(fields[3], fields[4]) {
		return nil, &Error{fmt.Sprintf("%s %s is an impossible day of month for the given month", patterns[3], patterns[4])}
	}
	return fields, nil
}

// Validate reports whether expression is a usable cron expression.
func Validate(expression string) bool {
	_, err := convert(expression)
	return err == nil
}
