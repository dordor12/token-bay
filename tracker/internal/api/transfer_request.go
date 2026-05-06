package api

import (
	"context"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// federationStartTransfer is reserved for the federation subsystem.
type federationStartTransfer interface {
	StartTransfer(ctx context.Context, req *tbproto.TransferRequest) (*tbproto.TransferProof, error)
}

func (r *Router) installTransferRequest() handlerFunc { return notImpl("transfer_request") }
