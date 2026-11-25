package wsclient

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Backoff is exponential: Min, Min*Factor, ... up to Max, with up to Jitter
// (a fraction of the delay) added so many clients do not retry in step.
type Backoff struct {
	Min, Max time.Duration
	Factor   float64
	Jitter   float64
}

// DefaultBackoff suits a long-lived upstream link.
var DefaultBackoff = Backoff{Min: 500 * time.Millisecond, Max: 30 * time.Second, Factor: 2, Jitter: 0.2}

// Delay is the wait before attempt number attempt (1 is the first retry).
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	factor := b.Factor
	if factor < 1 {
		factor = 2
	}
	d := float64(b.Min)
	for i := 1; i < attempt && d < float64(b.Max); i++ {
		d *= factor
	}
	if b.Max > 0 && d > float64(b.Max) {
		d = float64(b.Max)
	}
	if b.Jitter > 0 {
		d += d * b.Jitter * rand.Float64()
	}
	return time.Duration(d)
}

// ErrGaveUp wraps the last failure once MaxAttempts consecutive dials or
// sessions failed.
var ErrGaveUp = errors.New("wsclient: giving up reconnecting")

// Reconnect keeps a connection alive: it dials, hands the connection to
// serve, and dials again with backoff whenever serve returns an error or the
// peer goes away. A serve that returns nil ends the loop. The attempt
// counter starts over after a connection that stayed up for Stable.
type Reconnect struct {
	Dial        func(ctx context.Context) (*Conn, error)
	Serve       func(ctx context.Context, c *Conn) error
	Backoff     Backoff
	MaxAttempts int           // consecutive failures before giving up; 0 retries forever
	Stable      time.Duration // how long a connection must last to reset the counter
	Sleep       func(ctx context.Context, d time.Duration) error
}

// Run blocks until ctx ends, Serve returns nil, or attempts run out.
func (r Reconnect) Run(ctx context.Context) error {
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	attempt := 0
	for {
		started := time.Now()
		conn, err := r.Dial(ctx)
		if err == nil {
			err = r.Serve(ctx, conn)
			_ = conn.CloseNow()
			if err == nil {
				return nil
			}
			if r.Stable > 0 && time.Since(started) >= r.Stable {
				attempt = 0
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		if r.MaxAttempts > 0 && attempt >= r.MaxAttempts {
			return errors.Join(ErrGaveUp, err)
		}
		if err := sleep(ctx, r.Backoff.Delay(attempt)); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
