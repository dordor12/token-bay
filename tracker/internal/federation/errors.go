package federation

import "errors"

// Sentinel errors. Wrap with fmt.Errorf("...: %w", err) when adding context.
var (
	ErrPeerUnknown     = errors.New("federation: unknown peer")
	ErrPeerClosed      = errors.New("federation: peer connection closed")
	ErrSigInvalid      = errors.New("federation: invalid signature")
	ErrFrameTooLarge   = errors.New("federation: frame exceeds 1 MiB cap")
	ErrHandshakeFailed = errors.New("federation: handshake failed")
	ErrEquivocation    = errors.New("federation: equivocation detected")
	ErrPeerExists      = errors.New("federation: peer already registered")

	// ErrPeerNotConnected means the federation layer cannot reach the
	// requested peer (not in the active map; not in steady state).
	// Returned by Federation.StartTransfer and SendToPeer.
	ErrPeerNotConnected = errors.New("federation: peer not connected")

	// ErrTransferTimeout means a TRANSFER_PROOF was not received within
	// the configured TransferTimeout. The destination tracker's
	// StartTransfer returns this; the consumer's plugin retries.
	ErrTransferTimeout = errors.New("federation: transfer proof timeout")

	// ErrTransferDisabled means the federation subsystem was constructed
	// without a LedgerHooks dependency, so cross-region transfer is off.
	// Returned by StartTransfer and rejected on inbound transfer kinds.
	ErrTransferDisabled = errors.New("federation: transfer disabled (no LedgerHooks)")
)
