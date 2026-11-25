package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

var (
	errCanceled = context.Canceled
	errDeadline = context.DeadlineExceeded
)

// isNetworkError matches the connection failures Node treats as transient
// (reset, refused, timeout, DNS failure, unreachable, truncated stream).
func isNetworkError(err error) bool {
	for _, target := range []error{
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.EPIPE,
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EAGAIN, io.ErrUnexpectedEOF,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
