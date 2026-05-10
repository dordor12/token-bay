package reputation

import "testing"

func TestBreach_ImmediateAction(t *testing.T) {
	cases := []struct {
		b    BreachKind
		want State
	}{
		{BreachInvalidProofSignature, StateAudit},
		{BreachInconsistentSeederSig, StateAudit},
		{BreachReplayConfirmedNonce, StateAudit},
		{BreachConsecutiveProofRejects, StateAudit},
	}
	for _, c := range cases {
		if got := c.b.ImmediateAction(); got != c.want {
			t.Errorf("%v.ImmediateAction() = %v, want %v",
				c.b, got, c.want)
		}
	}
}

func TestBreach_String(t *testing.T) {
	if BreachInvalidProofSignature.String() != "invalid_proof_signature" {
		t.Error("BreachInvalidProofSignature label mismatch")
	}
	if BreachReplayConfirmedNonce.String() != "replay_confirmed_nonce" {
		t.Error("BreachReplayConfirmedNonce label mismatch")
	}
}
