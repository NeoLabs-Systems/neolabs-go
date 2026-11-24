// Package pwpolicy decides whether a new password is acceptable. It is a
// faithful port of the rule set the NeoLabs Node servers shipped, so the same
// password is accepted or rejected, with the same advice, by every app.
package pwpolicy

import (
	"strings"
	"unicode/utf16"
)

const (
	MinLength = 8
	minScore  = 3

	personalInfoFeedback  = "Avoid using your username or email in the password."
	commonPatternFeedback = "Avoid common words, repeated characters, or obvious sequences."
)

var commonPatterns = []string{"password", "123456", "qwerty", "letmein", "welcome", "admin", "neoagent"}

var sequences = []string{
	"abcdefghijklmnopqrstuvwxyz",
	"0123456789",
	"qwertyuiopasdfghjklzxcvbnm",
}

// Context carries the account details a password must not contain.
type Context struct {
	Username string
	Email    string
}

// Strength is the evaluation result; JSON names match the Node API.
type Strength struct {
	Score            int      `json:"score"`
	Label            string   `json:"label"`
	Length           int      `json:"length"`
	HasMinimumLength bool     `json:"hasMinimumLength"`
	IsAcceptable     bool     `json:"isAcceptable"`
	Feedback         []string `json:"feedback"`
}

// jsLength is String.prototype.length: UTF-16 code units.
func jsLength(s string) int {
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

func hasSequential(password string) bool {
	value := strings.ToLower(password)
	for _, seq := range sequences {
		for i := 0; i <= len(seq)-4; i++ {
			fragment := seq[i : i+4]
			if strings.Contains(value, fragment) || strings.Contains(value, reverse(fragment)) {
				return true
			}
		}
	}
	return false
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// hasRepeatedRuns mirrors /(.)\1{2,}/ without the `u` flag: three identical
// UTF-16 code units in a row, where `.` never matches a line terminator.
func hasRepeatedRuns(password string) bool {
	units := utf16.Encode([]rune(password))
	run := 1
	for i := 1; i < len(units); i++ {
		u := units[i]
		if u == units[i-1] && !isLineTerminator(u) {
			run++
			if run >= 3 {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

func isLineTerminator(u uint16) bool {
	return u == '\n' || u == '\r' || u == 0x2028 || u == 0x2029
}

func normalizeContext(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Evaluate scores a password.
func Evaluate(password string, ctx Context) Strength {
	normalized := strings.ToLower(password)
	length := jsLength(password)

	var lower, upper, digit, symbol int
	for _, u := range utf16.Encode([]rune(password)) {
		switch {
		case u >= 'a' && u <= 'z':
			lower++
		case u >= 'A' && u <= 'Z':
			upper++
		case u >= '0' && u <= '9':
			digit++
		default:
			symbol++
		}
	}
	unique := map[rune]struct{}{}
	for _, r := range password {
		unique[r] = struct{}{}
	}
	variety := 0
	for _, n := range []int{lower, upper, digit, symbol} {
		if n > 0 {
			variety++
		}
	}

	localPart := ctx.Email
	if i := strings.Index(localPart, "@"); i >= 0 {
		localPart = localPart[:i]
	}
	containsPersonal := false
	for _, id := range []string{normalizeContext(ctx.Username), normalizeContext(ctx.Email), normalizeContext(localPart)} {
		if len(id) >= 3 && strings.Contains(normalized, id) {
			containsPersonal = true
		}
	}
	commonPattern := false
	for _, p := range commonPatterns {
		if strings.Contains(normalized, p) {
			commonPattern = true
		}
	}
	sequential := hasSequential(password)
	repeated := hasRepeatedRuns(password)

	score := 0
	if length >= MinLength {
		score++
	}
	if length >= 12 {
		score++
	}
	if variety >= 3 {
		score++
	}
	if variety == 4 || length >= 16 || len(unique) >= 10 {
		score++
	}
	if containsPersonal || commonPattern || sequential || repeated {
		score--
	}
	if score < 0 {
		score = 0
	}
	if score > 4 {
		score = 4
	}

	feedback := []string{}
	if length < MinLength {
		feedback = append(feedback, "Use at least 8 characters.")
	} else if length < 12 {
		feedback = append(feedback, "A longer password is harder to guess.")
	}
	if variety < 3 && length < 16 {
		feedback = append(feedback, "Mix uppercase, lowercase, numbers, or symbols.")
	}
	if containsPersonal {
		feedback = append(feedback, personalInfoFeedback)
	}
	if commonPattern || sequential || repeated {
		feedback = append(feedback, commonPatternFeedback)
	}
	if len(feedback) == 0 && score >= minScore {
		feedback = append(feedback, "Password looks strong.")
	}

	label := "very weak"
	switch {
	case score >= 4:
		label = "strong"
	case score >= 3:
		label = "good"
	case score >= 2:
		label = "fair"
	case score >= 1:
		label = "weak"
	}
	return Strength{
		Score:            score,
		Label:            label,
		Length:           length,
		HasMinimumLength: length >= MinLength,
		IsAcceptable:     length >= MinLength && score >= minScore && !containsPersonal && !commonPattern && !sequential,
		Feedback:         feedback,
	}
}

// Error names the reason that actually rejected the password: personal info
// and common patterns beat general length and variety tips.
func Error(s Strength) string {
	detail := ""
	for _, want := range []string{personalInfoFeedback, commonPatternFeedback} {
		for _, f := range s.Feedback {
			if f == want {
				detail = f
				break
			}
		}
		if detail != "" {
			break
		}
	}
	if detail == "" && len(s.Feedback) > 0 {
		detail = s.Feedback[0]
	}
	if detail == "" {
		return "Password is too weak."
	}
	return "Password is too weak. " + detail
}
