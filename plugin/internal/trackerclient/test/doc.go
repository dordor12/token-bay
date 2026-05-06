// Package test holds component-level test scaffolding for the
// trackerclient package: fakeserver/ implements a minimal tracker for
// loopback-driven tests, and integration_test.go (build tag
// "integration") exercises the production QUIC transport against a
// real quic-go server. The package itself has no exported API.
package test
