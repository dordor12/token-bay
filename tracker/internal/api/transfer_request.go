package api

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// federationStartTransfer is the slice of the federation subsystem the
// transfer_request handler needs. The destination tracker's adapter in
// cmd/run_cmd.go maps tbproto.TransferRequest fields onto
// federation.StartTransferInput and reports the proof back as
// tbproto.TransferProof — keeping the api package free of the internal
// federation package's types.
type federationStartTransfer interface {
	StartTransfer(ctx context.Context, req *tbproto.TransferRequest) (*tbproto.TransferProof, error)
}

// installTransferRequest wires the live transfer_request handler when
// Deps.Federation is non-nil. Falls back to ErrNotImplemented otherwise.
// The handler verifies the consumer signature against the federation-
// canonical bytes of the equivalent TransferProofRequest as defense in
// depth — the source tracker re-checks on receive, but failing fast at
// the destination keeps spam off the federation wire.
func (r *Router) installTransferRequest() handlerFunc {
	if r.deps.Federation == nil {
		return notImpl("transfer_request")
	}
	if len(r.deps.TrackerPub) != ed25519.PublicKeySize {
		// MyTrackerID is needed to reconstruct the federation canonical
		// bytes for sig verification; without it the handler cannot
		// validate, so stay in the stub.
		return notImpl("transfer_request")
	}
	dstID := sha256.Sum256(r.deps.TrackerPub)

	return func(ctx context.Context, _ *RequestCtx, payload []byte) (*tbproto.RpcResponse, error) {
		var req tbproto.TransferRequest
		if err := proto.Unmarshal(payload, &req); err != nil {
			return nil, ErrInvalid("TransferRequest: " + err.Error())
		}
		if len(req.IdentityId) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("identity_id length %d, want 32", len(req.IdentityId)))
		}
		if len(req.Nonce) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("nonce length %d, want 32", len(req.Nonce)))
		}
		if len(req.SourceTrackerId) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("source_tracker_id length %d, want 32", len(req.SourceTrackerId)))
		}
		if len(req.ConsumerPub) != ed25519.PublicKeySize {
			return nil, ErrInvalid(fmt.Sprintf("consumer_pub length %d, want %d", len(req.ConsumerPub), ed25519.PublicKeySize))
		}
		if len(req.ConsumerSig) != ed25519.SignatureSize {
			return nil, ErrInvalid(fmt.Sprintf("consumer_sig length %d, want %d", len(req.ConsumerSig), ed25519.SignatureSize))
		}
		if req.Amount == 0 {
			return nil, ErrInvalid("amount must be > 0")
		}
		if req.Timestamp == 0 {
			return nil, ErrInvalid("timestamp must be > 0")
		}

		// Defense in depth: rebuild the canonical federation bytes the
		// source tracker will check, and verify the consumer signature
		// here. A bad sig at the api layer is dropped without consuming
		// federation send capacity.
		fedReq := &fed.TransferProofRequest{
			SourceTrackerId: req.SourceTrackerId,
			DestTrackerId:   dstID[:],
			IdentityId:      req.IdentityId,
			Amount:          req.Amount,
			Nonce:           req.Nonce,
			ConsumerPub:     req.ConsumerPub,
			Timestamp:       req.Timestamp,
		}
		canonical, err := fed.CanonicalTransferProofRequestPreSig(fedReq)
		if err != nil {
			return nil, fmt.Errorf("transfer_request: canonical: %w", err)
		}
		if !ed25519.Verify(req.ConsumerPub, canonical, req.ConsumerSig) {
			return nil, ErrUnauthenticated("consumer_sig invalid")
		}

		proof, err := r.deps.Federation.StartTransfer(ctx, &req)
		if err != nil {
			return nil, fmt.Errorf("transfer_request: federation: %w", err)
		}
		respBytes, err := proto.Marshal(proof)
		if err != nil {
			return nil, fmt.Errorf("transfer_request: marshal proof: %w", err)
		}
		return OkResponse(respBytes), nil
	}
}
