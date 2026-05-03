// Package proto: rpc-message metadata.
//
// MaxRPCPayloadSize bounds the proto-encoded payload bytes inside an
// RpcRequest or RpcResponse. The framing layer enforces a 1 MiB outer
// frame cap; this constant exists for callers that want to validate
// payloads before they reach the framer.
package proto

// MaxRPCPayloadSize is the largest allowed Payload byte length on an
// RpcRequest or RpcResponse. 1 MiB matches the outer-frame cap.
const MaxRPCPayloadSize = 1 << 20 // 1 MiB
