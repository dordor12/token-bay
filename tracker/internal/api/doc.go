// Package api owns the dispatch surface for tracker's plugin-facing
// wire protocol. It maps RpcMethod → handler, declares per-handler
// narrow interfaces over its concrete subsystems (ledger, registry,
// stunturn, broker, admission, federation), and provides encoders/
// decoders for the two server-initiated push-stream message pairs.
//
// internal/api MUST NOT import internal/server or any transport
// package. Server passes incoming RpcRequests through api.Router.Dispatch
// and uses api.PushAPI to encode/decode push-stream protos.
//
// See docs/superpowers/specs/tracker/2026-05-03-tracker-internal-api-server-design.md.
package api
