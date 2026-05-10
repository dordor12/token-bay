package reputation

import "testing"

func TestState_StringRoundTrip(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{StateOK, "OK"},
		{StateAudit, "AUDIT"},
		{StateFrozen, "FROZEN"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestState_AllowedTransitions(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StateOK, StateAudit, true},
		{StateOK, StateFrozen, true},    // severe categorical breach
		{StateAudit, StateOK, true},     // 48h cooldown
		{StateAudit, StateFrozen, true}, // 3 audits in 7d
		{StateFrozen, StateOK, false},   // operator-only, deferred
		{StateFrozen, StateAudit, false},
		{StateOK, StateOK, false},
		{StateAudit, StateAudit, false},
	}
	for _, c := range cases {
		if got := canTransition(c.from, c.to); got != c.want {
			t.Errorf("canTransition(%v→%v) = %v, want %v",
				c.from, c.to, got, c.want)
		}
	}
}
