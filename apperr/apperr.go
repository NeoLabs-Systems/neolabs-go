// Package apperr is the one error type the NeoLabs apps use for failures a
// client should see: an HTTP status, a message written for people and, when the
// client can branch on it, a stable code.
package apperr

import "errors"

// Error is a client-facing failure.
type Error struct {
	Status  int
	Message string
	Code    string
}

func (e *Error) Error() string { return e.Message }

// New builds an Error. code may be empty.
func New(status int, message, code string) *Error {
	return &Error{Status: status, Message: message, Code: code}
}

// As reports whether err is (or wraps) an *Error and returns it.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
