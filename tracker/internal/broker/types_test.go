package broker

import "testing"

func TestOutcome_NonZero(t *testing.T) {
	if OutcomeAdmit == OutcomeUnspecified {
		t.Fatal("OutcomeAdmit must not collide with zero value")
	}
}
