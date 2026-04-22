// Package proto holds the wire-format types exchanged across the Token-Bay network:
//   - broker_request envelopes (consumer → tracker)
//   - ledger Entry structs (written by trackers)
//   - federation messages (tracker ↔ tracker)
//
// Actual message types are added as they are implemented in subsequent
// feature plans. This file exists so the package compiles and carries
// the protocol version constant.
package proto

// ProtocolVersion is the current wire-format version.
// Bump when making breaking changes to any message in this package.
const ProtocolVersion uint16 = 1
