package llm

import (
	"context"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy is the transient-failure retry the harness wraps around provider
// calls: a blip retries the same model with a short backoff, and only when that
// is exhausted does the caller fall back to another model.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first
	BaseDelay   time.Duration // first backoff
	MaxDelay    time.Duration
	// MaxRetryAfter refuses a provider-requested wait longer than this: that is
	// exhausted quota, not a blip, and failing over beats parking the run.
	MaxRetryAfter time.Duration
	// IsRetryable overrides the default classification (IsTransient).
	IsRetryable func(error) bool
	// OnRetry is told before each wait so callers can show progress.
	OnRetry func(RetryInfo)
}

// RetryInfo describes one scheduled retry.
type RetryInfo struct {
	Attempt int
	Delay   time.Duration
	Err     error
}

// DefaultRetryPolicy returns the defaults of providerRetry.js.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second, MaxRetryAfter: 60 * time.Second}
}

// RetryPolicyFromEnv applies the NEOAGENT_AI_RETRY_* overrides read through
// getenv (clamped like Node: attempts 1..8, delays in milliseconds).
func RetryPolicyFromEnv(getenv func(string) string) RetryPolicy {
	p := DefaultRetryPolicy()
	number := func(name string, fallback, lo, hi float64) float64 {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return fallback
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return fallback
		}
		return math.Min(hi, math.Max(lo, n))
	}
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	p.MaxAttempts = int(number("NEOAGENT_AI_RETRY_MAX_ATTEMPTS", float64(p.MaxAttempts), 1, 8))
	p.BaseDelay = ms(number("NEOAGENT_AI_RETRY_BASE_MS", 500, 0, 60000))
	p.MaxDelay = ms(number("NEOAGENT_AI_RETRY_MAX_MS", 8000, 0, 120000))
	p.MaxRetryAfter = ms(number("NEOAGENT_AI_RETRY_MAX_RETRY_AFTER_MS", 60000, 0, 600000))
	return p
}

// BackoffDelay is exponential backoff with half jitter, like computeBackoffMs.
func BackoffDelay(attempt int, base, max time.Duration) time.Duration {
	exp := math.Min(float64(max), float64(base)*math.Pow(2, float64(attempt-1)))
	return time.Duration(math.Round(exp/2 + rand.Float64()*(exp/2)))
}

// Retry runs fn with transient-failure retries. fn receives the 1-based attempt.
func Retry[T any](ctx context.Context, p RetryPolicy, fn func(attempt int) (T, error)) (T, error) {
	var zero T
	isRetryable := p.IsRetryable
	if isRetryable == nil {
		isRetryable = IsTransient
	}
	attempts := max(p.MaxAttempts, 1)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := fn(attempt)
		if err == nil {
			return value, nil
		}
		if attempt >= attempts || !isRetryable(err) {
			return zero, err
		}
		wait := BackoffDelay(attempt, p.BaseDelay, p.MaxDelay)
		if e := AsError(err); e != nil && e.RetryAfter > 0 {
			if e.RetryAfter > p.MaxRetryAfter {
				return zero, err
			}
			wait = e.RetryAfter
		}
		if p.OnRetry != nil {
			p.OnRetry(RetryInfo{Attempt: attempt, Delay: wait, Err: err})
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
}
