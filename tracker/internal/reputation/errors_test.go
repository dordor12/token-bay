package reputation

import (
	"errors"
	"testing"
)

func TestErrSubsystemClosed_IsSentinel(t *testing.T) {
	if ErrSubsystemClosed == nil {
		t.Fatal("ErrSubsystemClosed must be non-nil")
	}
	if !errors.Is(ErrSubsystemClosed, ErrSubsystemClosed) {
		t.Fatal("errors.Is on the sentinel must succeed")
	}
}
