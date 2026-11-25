package apperr

import (
	"errors"
	"fmt"
	"testing"
)

func TestAs(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", New(404, "nope", "NOT_FOUND"))
	e, ok := As(err)
	if !ok || e.Status != 404 || e.Code != "NOT_FOUND" || e.Error() != "nope" {
		t.Fatalf("%v %v", e, ok)
	}
	if _, ok := As(errors.New("plain")); ok {
		t.Fatal("plain errors are not client errors")
	}
}
