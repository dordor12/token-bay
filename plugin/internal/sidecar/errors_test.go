package sidecar

import (
	"errors"
	"testing"
)

func TestSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrInvalidDeps, ErrAlreadyStarted) {
		t.Fatal("ErrInvalidDeps and ErrAlreadyStarted must be distinct")
	}
	if errors.Is(ErrAlreadyStarted, ErrClosed) {
		t.Fatal("ErrAlreadyStarted and ErrClosed must be distinct")
	}
	if errors.Is(ErrInvalidDeps, ErrClosed) {
		t.Fatal("ErrInvalidDeps and ErrClosed must be distinct")
	}
}

func TestSentinelsHaveStableMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrInvalidDeps, "sidecar: invalid deps"},
		{ErrAlreadyStarted, "sidecar: already started"},
		{ErrClosed, "sidecar: closed"},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.want {
			t.Errorf("got %q, want %q", tc.err.Error(), tc.want)
		}
	}
}
