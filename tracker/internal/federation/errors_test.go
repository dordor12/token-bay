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
	// Pointer-distinct sentinels can still share a message string, which
	// would confuse log readers and any caller switching on .Error().
	seen := make(map[string]int, len(cases))
	for i, e := range cases {
		if prev, dup := seen[e.Error()]; dup {
			t.Fatalf("error %d and %d share message %q", prev, i, e.Error())
		}
		seen[e.Error()] = i
	}
}
