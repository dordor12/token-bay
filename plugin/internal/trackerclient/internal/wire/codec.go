package wire

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// MarshalRequest packages a per-method payload into an RpcRequest envelope.
func MarshalRequest(method tbproto.RpcMethod, payload proto.Message) (*tbproto.RpcRequest, error) {
	if method == tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED {
		return nil, fmt.Errorf("wire: method 0 is reserved for heartbeat")
	}
	var pb []byte
	if payload != nil {
		var err error
		pb, err = proto.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("wire: marshal payload: %w", err)
		}
	}
	return &tbproto.RpcRequest{Method: method, Payload: pb}, nil
}

// UnmarshalResponse extracts the per-method payload from an RpcResponse.
// Caller passes a zero-valued *T; on OK status it is populated.
func UnmarshalResponse(resp *tbproto.RpcResponse, dst proto.Message) error {
	if resp == nil {
		return fmt.Errorf("wire: nil RpcResponse")
	}
	if dst == nil {
		return nil // caller doesn't care about the payload (Settle/Advertise/UsageReport ack)
	}
	if len(resp.Payload) == 0 {
		return nil
	}
	if err := proto.Unmarshal(resp.Payload, dst); err != nil {
		return fmt.Errorf("wire: unmarshal payload: %w", err)
	}
	return nil
}
