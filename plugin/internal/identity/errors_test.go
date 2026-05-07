package identity

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorsAreSentinels(t *testing.T) {
	cases := []error{
		ErrKeyNotFound,
		ErrKeyExists,
		ErrInvalidKey,
		ErrRecordNotFound,
		ErrInvalidRecord,
		ErrIneligibleAuth,
		ErrFingerprintMismatch,
	}
	for _, e := range cases {
		if e == nil {
			t.Fatal("nil sentinel")
		}
		wrapped := fmt.Errorf("ctx: %w", e)
		if !errors.Is(wrapped, e) {
			t.Fatalf("errors.Is broken for %v", e)
		}
	}
}
