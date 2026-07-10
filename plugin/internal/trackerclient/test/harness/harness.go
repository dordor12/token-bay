// Package harness wires an in-process fakeserver tracker to a loopback
// Transport so out-of-package tests — notably the cmd binaries under
// plugin/cmd/... — can drive a real trackerclient.Client against the fake
// without importing the double-internal loopback/transport packages (which
// the Go internal-package rule forbids outside the trackerclient subtree).
//
// It lives in its own package, separate from fakeserver, because fakeserver
// is imported by the trackerclient package's own tests; importing
// trackerclient back into fakeserver would form a test-time import cycle.
package harness

import (
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/shared/ids"
)

// Loopback returns a fakeserver reachable via the returned Transport at addr.
// Register per-method handlers on the Server before starting it with
// (*Server).Run, and build the Client with the returned Transport plus an
// endpoint whose Addr equals addr.
func Loopback(addr string) (*fakeserver.Server, trackerclient.Transport) {
	_, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen(addr, srv)
	return fakeserver.New(srv), drv
}
