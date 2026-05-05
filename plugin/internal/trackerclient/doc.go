// Package trackerclient is the plugin's long-lived mTLS QUIC client to a
// regional tracker. It exposes blocking unary RPCs for nine methods and
// dispatches three server-pushed streams (heartbeat, offers,
// settlements) to caller-provided handler interfaces.
//
// The client owns connection lifecycle and reconnect; callers see only
// blocking RPC methods plus a Status / WaitConnected surface.
//
// See docs/superpowers/specs/plugin/2026-05-02-trackerclient-design.md.
package trackerclient
