package broker

import (
	"errors"
	"testing"
)

func TestErrorsAreSentinel(t *testing.T) {
	sentinels := []error{
		ErrInsufficientCredits, ErrDuplicateReservation, ErrUnknownReservation,
		ErrUnknownModel, ErrIllegalTransition, ErrUnknownRequest,
		ErrSeederMismatch, ErrModelMismatch, ErrCostOverspend,
		ErrSeederSigInvalid, ErrDuplicateSettle, ErrUnknownPreimage,
	}
	for _, e := range sentinels {
		if errors.Unwrap(e) != nil {
			t.Errorf("%v should be a leaf sentinel", e)
		}
		if e.Error() == "" {
			t.Errorf("%v has empty message", e)
		}
	}
}
