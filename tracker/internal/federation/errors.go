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
)
