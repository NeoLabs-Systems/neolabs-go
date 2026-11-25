// Package oauthclient is the HTTP and OAuth2 plumbing an app needs to talk to
// third-party APIs on behalf of a signed-in account: bounded JSON requests with
// one retry for idempotent calls, provider errors with secrets scrubbed, PKCE,
// authorization URLs and refresh-on-expiry for stored credentials.
//
// It never decides which hosts are reachable: pass an HTTP client that already
// enforces that (see package ssrf).
package oauthclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	DefaultTimeout  = 15 * time.Second
	DefaultMaxBytes = 10 << 20
	// MaxRetryDelay is the longest wait before a retry; a service asking for
	// more is reported as failed instead.
	MaxRetryDelay = 10 * time.Second
)

// Request describes one call. At most one of JSON, Form and Body is used, in
// that order.
type Request struct {
	Method string // default GET
	URL    string
	Header map[string]string
	JSON   any
	Form   [][2]string // ordered key/value pairs; blank-less pairs are the caller's choice
	Body   []byte
	// NoRetry turns off the single retry that GET, HEAD and OPTIONS get.
	NoRetry  bool
	Timeout  time.Duration
	MaxBytes int64
}

// Response is a completed call, whatever its status.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Error is a provider answering with a failure status, or with the
// {"ok": false} envelope some APIs (Slack) use under a 200.
type Error struct {
	Status  int
	Message string
	// Data is the parsed JSON body when it was an object.
	Data   map[string]any
	Header http.Header
}

func (e *Error) Error() string { return e.Message }

// Client makes bounded requests. The zero Service reads "OAuth provider".
type Client struct {
	HTTP    *http.Client
	Service string
	// TimeoutCode and TooLargeCode are the Code() of the errors for a request
	// over its deadline and a response over its size limit; empty is fine.
	TimeoutCode, TooLargeCode string
	// Sleep waits between retries; nil uses a real timer. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c *Client) service() string {
	if s := strings.TrimSpace(c.Service); s != "" {
		return s
	}
	return "OAuth provider"
}

// Named returns a copy that reports errors under service.
func (c *Client) Named(service string) *Client {
	cp := *c
	cp.Service = service
	return &cp
}

// Do performs the request once and returns whatever the service answered. A
// body over the size limit and a request over its deadline are errors.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := r.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	contentType := ""
	switch {
	case r.JSON != nil:
		raw, err := marshalJS(r.JSON)
		if err != nil {
			return nil, err
		}
		body, contentType = bytes.NewReader(raw), "application/json"
	case r.Form != nil:
		body, contentType = strings.NewReader(encodeForm(r.Form)), "application/x-www-form-urlencoded"
	case r.Body != nil:
		body = bytes.NewReader(r.Body)
	}

	ctx, cancel := context.WithTimeoutCause(ctx, timeout, &timeoutError{service: c.service(), timeout: timeout, code: c.TimeoutCode})
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.wrap(ctx, err)
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, &tooLargeError{service: c.service(), limit: limit, code: c.TooLargeCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, c.wrap(ctx, err)
	}
	if int64(len(data)) > limit {
		return nil, &tooLargeError{service: c.service(), limit: limit, code: c.TooLargeCode}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

type timeoutError struct {
	service string
	timeout time.Duration
	code    string
}

func (e *timeoutError) Code() string { return e.code }

func (e *timeoutError) Error() string {
	return fmt.Sprintf("%s request timed out after %dms.", e.service, e.timeout.Milliseconds())
}

type tooLargeError struct {
	service string
	limit   int64
	code    string
}

func (e *tooLargeError) Code() string { return e.code }

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("%s response exceeded the %d-byte safety limit.", e.service, e.limit)
}

// wrap turns the deadline's cause into the readable timeout error.
func (c *Client) wrap(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		var te *timeoutError
		if errors.As(cause, &te) {
			return te
		}
	}
	return err
}

// JSON performs the request and returns the decoded-later body: the raw JSON
// of a success, or "null" when the service sent none or something that is not
// JSON. Failure statuses and {"ok": false} bodies are *Error.
func (c *Client) JSON(ctx context.Context, r Request) (json.RawMessage, error) {
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	attempts := 2
	if r.NoRetry || (method != "" && method != "GET" && method != "HEAD" && method != "OPTIONS") {
		attempts = 1
	}
	for attempt := 1; ; attempt++ {
		raw, err := c.jsonOnce(ctx, r)
		if err == nil {
			return raw, nil
		}
		if ctx.Err() != nil || attempt >= attempts {
			return nil, err
		}
		delay, ok := retryDelay(err, attempt)
		if !ok {
			return nil, err
		}
		sleep := c.Sleep
		if sleep == nil {
			sleep = sleepContext
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

func (c *Client) jsonOnce(ctx context.Context, r Request) (json.RawMessage, error) {
	resp, err := c.Do(ctx, r)
	if err != nil {
		return nil, err
	}
	var parsed any
	text := string(resp.Body)
	if len(bytes.TrimSpace(resp.Body)) > 0 {
		if json.Unmarshal(resp.Body, &parsed) != nil {
			parsed = nil
		}
	}
	data, _ := parsed.(map[string]any)
	notOK := false
	if data != nil {
		if ok, has := data["ok"]; has && ok == false {
			notOK = true
		}
	}
	if resp.Status < 200 || resp.Status > 299 || notOK {
		raw := firstTruthy(data["error_description"], data["error"], data["message"])
		msg := ""
		switch {
		case raw != nil:
			msg = jsString(raw)
		case text != "":
			msg = text
		default:
			msg = fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status))
		}
		return nil, &Error{
			Status:  resp.Status,
			Message: c.service() + " request failed: " + SanitizeRemote(msg),
			Data:    data,
			Header:  resp.Header,
		}
	}
	if parsed == nil {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(bytes.TrimSpace(resp.Body)), nil
}

// retryableStatus are the statuses worth a second try.
var retryableStatus = map[int]bool{408: true, 409: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true, 520: true, 521: true, 522: true, 524: true, 529: true}

func retryDelay(err error, attempt int) (time.Duration, bool) {
	var pe *Error
	var wait time.Duration
	haveWait := false
	rateLimited := false
	transient := false
	switch {
	case errors.As(err, &pe):
		wait, haveWait = RetryAfter(pe.Header, time.Now())
		rateLimited = (pe.Status == 403 && haveWait) ||
			strings.EqualFold(jsString(pe.Data["error"]), "ratelimited")
		transient = retryableStatus[pe.Status]
	default:
		var te *timeoutError
		var ne net.Error
		transient = errors.As(err, &te) || (errors.As(err, &ne) && ne.Timeout()) ||
			errors.Is(err, io.ErrUnexpectedEOF) || isConnectionError(err)
	}
	if !rateLimited && !transient {
		return 0, false
	}
	if !haveWait {
		wait = backoff(attempt, 250*time.Millisecond, 2*time.Second)
	}
	if wait > MaxRetryDelay {
		return 0, false
	}
	return wait, true
}

func isConnectionError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op)
}

func backoff(attempt int, base, limit time.Duration) time.Duration {
	exp := math.Min(float64(limit), float64(base)*math.Pow(2, float64(attempt-1)))
	return time.Duration(math.Round(exp/2 + rand.Float64()*(exp/2)))
}

// RetryAfter reads Retry-After-Ms and Retry-After (seconds or an HTTP date).
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			return time.Duration(n * float64(time.Millisecond)), true
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, t.Sub(now)), true
	}
	return 0, false
}

var (
	bearerRE = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
	secretRE = regexp.MustCompile(`(?i)\b(access_token|refresh_token|client_secret|authorization|token)\b\s*[:=]\s*["']?[^\s,"'}\]]+`)
)

// SanitizeRemote shortens a provider's error text and hides tokens in it.
func SanitizeRemote(value string) string {
	value = sliceUTF16(value, 1000)
	value = bearerRE.ReplaceAllString(value, "$1 [redacted]")
	value = secretRE.ReplaceAllString(value, "$1=[redacted]")
	return strings.TrimSpace(value)
}

// sliceUTF16 is JavaScript's slice(0, n): n UTF-16 code units.
func sliceUTF16(s string, n int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return string(utf16.Decode(u[:n]))
}

// firstTruthy is a || b || c over JSON values.
func firstTruthy(values ...any) any {
	for _, v := range values {
		switch t := v.(type) {
		case nil:
		case string:
			if t != "" {
				return v
			}
		case bool:
			if t {
				return v
			}
		case float64:
			if t != 0 {
				return v
			}
		default:
			return v
		}
	}
	return nil
}

// jsString is JavaScript's String(value) for a decoded JSON value.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// marshalJS is JSON.stringify: no HTML escaping and no trailing newline.
func marshalJS(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalJS exposes marshalJS for callers building bodies themselves.
func MarshalJS(v any) ([]byte, error) { return marshalJS(v) }

// ---- URL building -----------------------------------------------------------

// formEscape is URLSearchParams' encoding: everything but letters, digits and
// "*-._" is percent-encoded and a space is "+".
func formEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

func encodeForm(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = formEscape(p[0]) + "=" + formEscape(p[1])
	}
	return strings.Join(parts, "&")
}

// Query is an ordered list of query parameters.
type Query [][2]string

// Set replaces the value of key, or appends it.
func (q *Query) Set(key, value string) {
	for i, p := range *q {
		if p[0] == key {
			(*q)[i][1] = value
			return
		}
	}
	*q = append(*q, [2]string{key, value})
}

// EncodeQuery is URLSearchParams.toString for ordered pairs.
func EncodeQuery(q Query) string { return encodeForm(q) }

// SetParams sets each parameter on rawURL like URL.searchParams.set: an
// existing key is replaced, a new one appended, blank values are kept.
func SetParams(rawURL string, params Query) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	var q Query
	if u.RawQuery != "" {
		for _, part := range strings.Split(u.RawQuery, "&") {
			k, v, _ := strings.Cut(part, "=")
			k, _ = url.QueryUnescape(k)
			v, _ = url.QueryUnescape(v)
			q = append(q, [2]string{k, v})
		}
	}
	for _, p := range params {
		q.Set(p[0], p[1])
	}
	u.RawQuery = encodeForm(q)
	return u.String(), nil
}

// AppendQuery adds key/value pairs to rawURL like SetParams, skipping blank
// values and trimming the rest.
func AppendQuery(rawURL string, kv ...string) (string, error) {
	var params Query
	for i := 0; i+1 < len(kv); i += 2 {
		if v := strings.TrimSpace(kv[i+1]); v != "" {
			params = append(params, [2]string{kv[i], v})
		}
	}
	return SetParams(rawURL, params)
}

// JoinScopes is the space-separated, de-duplicated scope string.
func JoinScopes(scopes ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, s := range scopes {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// ---- PKCE -------------------------------------------------------------------

// CodeChallenge is the S256 challenge of a PKCE verifier.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---- Credentials ------------------------------------------------------------

// Credentials are what is stored for a connected account: the provider's token
// response plus bookkeeping, as a JSON object.
type Credentials map[string]any

// String reads a string member, or "".
func (c Credentials) String(key string) string {
	s, _ := c[key].(string)
	return s
}

// Clone copies the top level.
func (c Credentials) Clone() Credentials {
	out := make(Credentials, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Merge overlays updated on existing; a refresh response that omits the
// refresh token keeps the old one.
func Merge(existing, updated Credentials) Credentials {
	merged := existing.Clone()
	for k, v := range updated {
		merged[k] = v
	}
	if !truthy(merged["refresh_token"]) && truthy(existing["refresh_token"]) {
		merged["refresh_token"] = existing["refresh_token"]
	}
	return merged
}

func truthy(v any) bool { return firstTruthy(v) != nil }

// ExpiresAt is the ISO time an access token lives until. When expiresIn is not
// a positive number: nil with nullIfInvalid, one hour from now otherwise.
func ExpiresAt(expiresIn any, nullIfInvalid bool, now time.Time) any {
	seconds, ok := toNumber(expiresIn)
	if !ok || seconds <= 0 {
		if nullIfInvalid {
			return nil
		}
		return ISO(now.Add(time.Hour))
	}
	return ISO(now.Add(time.Duration(seconds * float64(time.Second))))
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, !math.IsNaN(t) && !math.IsInf(t, 0)
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil && strings.TrimSpace(t) != ""
	}
	return 0, false
}

// ISO is JavaScript's toISOString.
func ISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ExpiresSoon reports whether credentials' expires_at is within skew of now.
func ExpiresSoon(c Credentials, skew time.Duration, now time.Time) bool {
	raw, _ := c["expires_at"].(string)
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	return !t.After(now.Add(skew))
}

// WithRefresh runs request with the stored credentials, refreshing them first
// when they expire within a minute and once more when request fails in a way
// shouldRetry accepts (nil: an HTTP 401 while a refresh token exists). The
// credentials in effect after the call are returned so the caller can store
// them.
func WithRefresh[T any](
	ctx context.Context,
	creds Credentials,
	refresh func(context.Context, Credentials) (Credentials, error),
	request func(Credentials) (T, error),
	shouldRetry func(error, Credentials) bool,
) (T, Credentials, error) {
	var zero T
	if ExpiresSoon(creds, time.Minute, time.Now()) {
		next, err := refresh(ctx, creds)
		if err != nil {
			return zero, creds, err
		}
		creds = next
	}
	out, err := request(creds)
	if err == nil {
		return out, creds, nil
	}
	retry := false
	if shouldRetry != nil {
		retry = shouldRetry(err, creds)
	} else {
		var pe *Error
		retry = errors.As(err, &pe) && pe.Status == 401 && strings.TrimSpace(creds.String("refresh_token")) != ""
	}
	if !retry {
		return zero, creds, err
	}
	next, rerr := refresh(ctx, creds)
	if rerr != nil {
		return zero, creds, rerr
	}
	out, err = request(next)
	return out, next, err
}
