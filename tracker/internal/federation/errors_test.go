package federation_test

import (
	"errors"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestErrSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	cases := []error{
		federation.ErrPeerUnknown,
		federation.ErrPeerClosed,
		federation.ErrSigInvalid,
		federation.ErrFrameTooLarge,
		federation.ErrHandshakeFailed,
		federation.ErrEquivocation,
	}
	for i, a := range cases {
		for j, b := range cases {
			if i != j && errors.Is(a, b) {
				t.Fatalf("error %d aliases %d", i, j)
			}
		}
	}
}
