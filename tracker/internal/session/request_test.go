package session

import "testing"

func TestState_String(t *testing.T) {
	cases := map[State]string{
		StateUnspecified: "unspecified",
		StateSelecting:   "selecting",
		StateAssigned:    "assigned",
		StateServing:     "serving",
		StateCompleted:   "completed",
		StateFailed:      "failed",
		State(99):        "unspecified",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestIsAllowedTransition(t *testing.T) {
	cases := []struct {
		from, to State
		ok       bool
	}{
		{StateSelecting, StateAssigned, true},
		{StateSelecting, StateFailed, true},
		{StateAssigned, StateServing, true},
		{StateAssigned, StateFailed, true},
		{StateServing, StateCompleted, true},
		{StateServing, StateFailed, true},

		{StateSelecting, StateServing, false},   // skips ASSIGNED
		{StateSelecting, StateCompleted, false}, // skips ASSIGNED+SERVING
		{StateAssigned, StateCompleted, false},  // skips SERVING
		{StateCompleted, StateServing, false},   // backwards
		{StateFailed, StateCompleted, false},    // terminal
		{StateFailed, StateServing, false},      // terminal
		{StateUnspecified, StateSelecting, false},
	}
	for _, c := range cases {
		if got := IsAllowedTransition(c.from, c.to); got != c.ok {
			t.Errorf("IsAllowedTransition(%v, %v) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}
