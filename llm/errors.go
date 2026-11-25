package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Stable error codes shared by the providers.
const (
	CodeModelSelectionRequired = "MODEL_SELECTION_REQUIRED"
	CodeDegenerateOutput       = "MODEL_DEGENERATE_OUTPUT"
	CodeHTTPTimeout            = "HTTP_TIMEOUT"
	CodeResponseTooLarge       = "HTTP_RESPONSE_TOO_LARGE"
)

var (
	redactAuthScheme = regexp.MustCompile(`(?i)\b(Bearer|Basic|token)\s+[A-Za-z0-9._~+/=-]+`)
	redactQueryKey   = regexp.MustCompile(`(?i)([?&](?:key|api_key|access_token)=)[^&\s]+`)
	redactAssignment = regexp.MustCompile(`(?i)\b(api[_-]?key|access_token|refresh_token|authorization)\b\s*[:=]\s*["']?[^\s,"'}\]]+`)
)

// SanitizeDetail shortens provider error text to 2000 UTF-16 units and masks
// credentials in it, so it is safe to show and to log.
func SanitizeDetail(value string) string {
	if value == "" {
		value = "Unknown provider error"
	}
	if units := utf16.Encode([]rune(value)); len(units) > 2000 {
		value = string(utf16.Decode(units[:2000]))
	}
	value = redactAuthScheme.ReplaceAllString(value, "${1} [redacted]")
	value = redactQueryKey.ReplaceAllString(value, "${1}[redacted]")
	return redactAssignment.ReplaceAllString(value, "${1}=[redacted]")
}

// wrappedError is an *Error that also keeps the failure it was made from, so
// errors.As finds both (for example a degenerate-output verdict under a prefix).
type wrappedError struct {
	err   *Error
	cause error
}

func (w *wrappedError) Error() string   { return w.err.Message }
func (w *wrappedError) Unwrap() []error { return []error{w.err, w.cause} }

// AsError returns the *Error inside err, if there is one.
func AsError(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

// WrapError prefixes a failure the way the Node providers do:
// "<prefix>: <sanitized detail>". Status, code and retry hints of the original
// are kept. Context cancellation passes through untouched.
func WrapError(provider string, err error, prefix string) error {
	return wrapWithDetail(provider, err, prefix, err.Error())
}

// WrapErrorDetail is WrapError with an explicit detail text.
func WrapErrorDetail(provider string, err error, prefix, detail string) error {
	return wrapWithDetail(provider, err, prefix, detail)
}

func wrapWithDetail(provider string, err error, prefix, detail string) error {
	if IsAbort(err) {
		return err
	}
	out := &Error{Provider: provider, Message: prefix + ": " + SanitizeDetail(detail)}
	if inner := AsError(err); inner != nil {
		out.Status, out.Code, out.Retryable, out.RetryAfter = inner.Status, inner.Code, inner.Retryable, inner.RetryAfter
	} else {
		out.Retryable = IsTransient(err)
	}
	return &wrappedError{err: out, cause: err}
}

// IsAbort reports a cancelled or timed-out context.
func IsAbort(err error) bool {
	return errors.Is(err, errCanceled) || errors.Is(err, errDeadline)
}

// RetryableStatus is the set of HTTP statuses worth retrying.
func RetryableStatus(status int) bool {
	switch status {
	case 408, 409, 425, 429, 500, 502, 503, 504, 520, 521, 522, 524, 529:
		return true
	}
	return false
}

// RetryAfterFrom reads retry-after-ms or retry-after (seconds or HTTP date).
func RetryAfterFrom(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := strings.TrimSpace(h.Get("retry-after"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// StatusError builds the error an HTTP failure produces. The message follows the
// vendor SDKs: "<status> <error.message>", or "<status> <json of error>" when
// the error has no message, "<status> status code (no body)" when empty, and the
// raw text when the body is not JSON. bodyIsError is true for APIs whose whole
// body is the error object (Anthropic); otherwise the "error" member is.
func StatusError(provider string, status int, h http.Header, body []byte, bodyIsError bool) *Error {
	var errRaw json.RawMessage
	var rawText string
	if json.Valid(body) {
		errRaw = body
		if !bodyIsError {
			errRaw = nil
			var top map[string]json.RawMessage
			if json.Unmarshal(body, &top) == nil {
				errRaw = top["error"]
			}
		}
	} else {
		rawText = string(body)
	}
	out := &Error{Provider: provider, Status: status, Retryable: RetryableStatus(status)}
	if status != 0 {
		out.RetryAfter = RetryAfterFrom(h, time.Now())
	}
	var fields map[string]json.RawMessage
	if len(errRaw) > 0 {
		_ = json.Unmarshal(errRaw, &fields)
	}
	msg := rawText
	if len(errRaw) > 0 && string(errRaw) != "null" {
		msg = compactJSON(errRaw)
		if m, ok := fields["message"]; ok && rawTruthy(m) {
			var s string
			if json.Unmarshal(m, &s) == nil {
				msg = s
			} else {
				msg = compactJSON(m)
			}
		}
	}
	switch {
	case status != 0 && msg != "":
		out.Message = strconv.Itoa(status) + " " + msg
	case status != 0:
		out.Message = strconv.Itoa(status) + " status code (no body)"
	case msg != "":
		out.Message = msg
	default:
		out.Message = "(no status code or body)"
	}
	if fields != nil {
		var code any
		_ = json.Unmarshal(fields["code"], &code)
		if s, ok := code.(string); ok {
			out.Code = s
		}
		// A vendor error without an HTTP status (an SSE error event, an error
		// inside a 200) often carries one, e.g. {"code": 429}; retry decisions use it.
		if status == 0 {
			var st any
			_ = json.Unmarshal(fields["status"], &st)
			if n := numericStatus(st); n != 0 {
				out.Status = n
			} else if n := numericStatus(code); n != 0 {
				out.Status = n
			}
			out.Retryable = RetryableStatus(out.Status)
		}
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// rawTruthy is JavaScript truthiness of a JSON value.
func rawTruthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return truthy(v)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	}
	return true
}

var envelopeStatus = regexp.MustCompile(`(?i)\b(overloaded|rate limit|timed? ?out|timeout|temporarily unavailable|connection (?:reset|refused|error)|socket hang up|network (?:error|timeout)|service unavailable)\b`)

// IsTransient reports whether a failure is worth retrying on the same model. It
// uses the same evidence as the Node harness: an HTTP status (also one embedded
// in a JSON envelope in the message), a network error, or typical wording.
func IsTransient(err error) bool {
	if err == nil || IsAbort(err) {
		return false
	}
	if e := AsError(err); e != nil {
		if e.Status != 0 {
			return RetryableStatus(e.Status)
		}
		if e.Retryable {
			return true
		}
	}
	if status := envelopeStatusOf(err.Error()); status != 0 {
		return RetryableStatus(status)
	}
	if isNetworkError(err) {
		return true
	}
	return envelopeStatus.MatchString(err.Error())
}

// envelopeStatusOf finds a status inside a "Prefix: {json}" message.
func envelopeStatusOf(message string) int {
	start := strings.Index(message, "{")
	end := strings.LastIndex(message, "}")
	if start == -1 || end <= start {
		return 0
	}
	var env map[string]any
	if json.Unmarshal([]byte(message[start:end+1]), &env) != nil {
		return 0
	}
	candidates := []any{env["status"]}
	if inner, ok := env["error"].(map[string]any); ok {
		candidates = append(candidates, inner["status"], inner["code"])
	}
	for _, c := range candidates {
		if n := numericStatus(c); n != 0 {
			return n
		}
	}
	return 0
}

// numericStatus reads an HTTP status from a JSON number or numeric string.
func numericStatus(v any) int {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0
		}
		n = f
	default:
		return 0
	}
	if n >= 100 && n < 600 {
		return int(n)
	}
	return 0
}
