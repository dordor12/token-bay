package harness

import "testing"

// TestLoopback_ReturnsWiredServerAndTransport exercises Loopback so the
// package is not a no-test package. Beyond the intrinsic value, a no-test
// package under `go test -coverprofile ./...` in CI's toolchain-switch
// environment can trip `go: no such tool "covdata"`; a real test avoids
// that path.
func TestLoopback_ReturnsWiredServerAndTransport(t *testing.T) {
	srv, tr := Loopback("loopback-test:1")
	if srv == nil {
		t.Fatal("Loopback returned a nil *fakeserver.Server")
	}
	if tr == nil {
		t.Fatal("Loopback returned a nil Transport")
	}
}
