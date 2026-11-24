// Package publicerr keeps technical failure detail away from people who are
// not administrators. Every API response passes through it: a non-admin sees
// only text written for people, never an exception message, a file path or a
// SQL error. Rules are a faithful port of the Node servers.
package publicerr

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// Generic is what a non-admin reads instead of technical detail.
const Generic = "Something went wrong. Please try again."

const (
	maxReadable = 300
	maxDepth    = 10
)

var technical = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|\n)\s*(?:at\s+\S|#\d+\s)`),
	regexp.MustCompile(`\b\w*(?:Error|Exception):`),
	regexp.MustCompile(`\b(?:ECONN\w+|ENOTFOUND|ETIMEDOUT|EAI_AGAIN|EPIPE|ENOENT|EACCES|EPERM|EADDR\w+|EHOSTUNREACH|ENETUNREACH|ERR_[A-Z0-9_]+|SQLITE_[A-Z0-9_]+)\b`),
	regexp.MustCompile(`\[(?:path|module|app)\]|node_modules|(?:^|[\s'"(])(?:~/|/Users/|/home/|/var/|/tmp/|/etc/|[A-Za-z]:\\)`),
	regexp.MustCompile(`(?i)stack trace|traceback|cannot read propert|is not a function|is not defined|unexpected token|not valid json|getaddrinfo|socket hang up|fetch failed|constraint failed|no such (?:table|column)|\bsqlite\b|\bsql\b`),
	regexp.MustCompile(`(?i)\bHTTP\s*\d{3}\b|\bstatus(?: code)?:?\s*\d{3}\b|^\d{3}\b`),
	regexp.MustCompile(`(?i)[a-z]_(?:error|exception|failed)\b`),
	regexp.MustCompile(`[{\[]\s*"`),
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// LooksTechnical reports whether text reads like a stack trace, driver error,
// path or other server internals. Kept in step with `looksTechnical` in the
// Flutter client's error_text.dart.
func LooksTechnical(text string) bool {
	value := strings.TrimSpace(text)
	if value == "" {
		return false
	}
	if strings.Contains(value, "\n") || utf16Len(value) > maxReadable {
		return true
	}
	for _, re := range technical {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

var (
	systemCode = regexp.MustCompile(`^(?:E[A-Z0-9]+|ERR_[A-Z0-9_]+|SQLITE_[A-Z0-9_]+)$`)
	appCode    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

// PublicCode returns code when it is an application error code that is safe to
// hand to clients, or "" for Node/driver codes such as ENOENT or SQLITE_BUSY.
func PublicCode(code string) string {
	if appCode.MatchString(code) && !systemCode.MatchString(code) {
		return code
	}
	return ""
}

var (
	nodeModules = regexp.MustCompile(`(?:^|[\s'"(\[])\S*?[\\/]node_modules(?:[\\/]\S*)?`)
	unixPath    = regexp.MustCompile(`(^|[\s'"(\[])/(?:[^\s'"\])]+/){2,}[^\s'"\])]+`)
	winPath     = regexp.MustCompile(`(^|[\s'"(\[])(?:[A-Za-z]:[\\/](?:[^\s'"\])]+[\\/])+[^\s'"\])]+|[\\/]{2}[^\s'"\\/\])]+[\\/][^\s'"\])]+(?:[\\/][^\s'"\])]+)+)`)
	leadChar    = regexp.MustCompile(`^[\s'"(\[]`)
)

// Sanitize hides the home directory, the app directory and absolute paths in a
// message. home and root may be empty.
func Sanitize(message, home, root string) string {
	msg := message
	if msg == "" || msg == "[object Object]" {
		msg = "An unexpected error occurred"
	}
	if home != "" {
		msg = strings.ReplaceAll(msg, home, "~")
	}
	if root != "" {
		msg = strings.ReplaceAll(msg, root, "[app]")
	}
	msg = nodeModules.ReplaceAllStringFunc(msg, func(m string) string {
		return leadChar.FindString(m) + "[module]"
	})
	msg = unixPath.ReplaceAllString(msg, "${1}[path]")
	msg = winPath.ReplaceAllString(msg, "${1}[path]")
	if msg = strings.TrimSpace(msg); msg == "" {
		return "An unexpected error occurred"
	}
	return msg
}

// Failure describes an error for NonAdminMessage.
type Failure struct {
	Message  string
	Status   int    // 0 when unknown
	Code     string // application or system code
	IsString bool   // the failure was a plain string, written for people
}

// NonAdminMessage is the failure text a non-admin may read: only text written
// for people (a plain string, a public code or a 4xx status) survives, and only
// if it does not look technical.
func NonAdminMessage(f Failure, home, root string) string {
	written := f.IsString || PublicCode(f.Code) != "" || (f.Status >= 400 && f.Status < 500)
	if !written {
		return Generic
	}
	msg := Sanitize(f.Message, home, root)
	if LooksTechnical(msg) {
		return Generic
	}
	return msg
}

var errorTextKeys = map[string]bool{"error": true, "last_error": true, "lastError": true, "errorMessage": true, "error_message": true}
var serverFailureDetailKeys = []string{"message", "detail", "details", "stack"}

func redactNested(value any, depth int) any {
	if depth > maxDepth {
		return value
	}
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactNested(item, depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if s, ok := item.(string); ok && errorTextKeys[k] && LooksTechnical(s) {
				out[k] = Generic
				continue
			}
			out[k] = redactNested(item, depth+1)
		}
		return out
	}
	return value
}

// RedactBody returns a decoded JSON response as a non-admin may see it. A 5xx
// says nothing about its cause; any other response keeps its text unless that
// text is technical. The input is not modified.
func RedactBody(body any, status int) any {
	redacted := redactNested(body, 0)
	obj, ok := redacted.(map[string]any)
	if status < 500 || !ok {
		return redacted
	}
	for _, k := range serverFailureDetailKeys {
		delete(obj, k)
	}
	if _, isString := obj["error"].(string); isString {
		obj["error"] = Generic
	}
	return obj
}
