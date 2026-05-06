// Package server owns the QUIC listener, mTLS termination, per-peer
// state, and the stream lifecycle for tracker's plugin-facing wire
// protocol. It is the only package in tracker that imports quic-go and
// crypto/tls.
//
// See docs/superpowers/specs/tracker/2026-05-03-tracker-internal-api-server-design.md.
package server
