package fakeserver

import "testing"

// Smoke test: ensure New() with a nil conn returns a usable shell with
// the documented defaults. Real behavior coverage lives in the parent
// trackerclient package's component tests; this exists primarily so that
// `go test -coverprofile` routes fakeserver through the normal test
// binary path (the alternative path requires the covdata tool, which
// is sometimes absent from auto-bootstrapped Go toolchains in CI).
func TestNewDefaults(t *testing.T) {
	s := New(nil)
	if s == nil {
		t.Fatal("New(nil) returned nil")
	}
	if s.MaxFrameSz == 0 {
		t.Fatal("New(nil) did not set MaxFrameSz default")
	}
}
