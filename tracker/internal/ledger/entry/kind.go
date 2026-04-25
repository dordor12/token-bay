package entry

import (
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Kind is the Go-typed mirror of tbproto.EntryKind. Using a Go enum here
// (instead of leaking the protobuf type into broker/ledger code) keeps the
// generated package surface narrow and gives a real exhaustive-switch signal.
type Kind uint8

const (
	KindUsage Kind = iota + 1
	KindTransferOut
	KindTransferIn
	KindStarterGrant
)

// String returns the human-readable kind name. Used for logs and errors.
func (k Kind) String() string {
	switch k {
	case KindUsage:
		return "usage"
	case KindTransferOut:
		return "transfer_out"
	case KindTransferIn:
		return "transfer_in"
	case KindStarterGrant:
		return "starter_grant"
	default:
		return "unknown"
	}
}

// Proto returns the wire-format enum value for k. Panics on unknown
// because every Kind value defined in this package has a mapping;
// constructing a Kind outside the constants is a programmer error.
func (k Kind) Proto() tbproto.EntryKind {
	switch k {
	case KindUsage:
		return tbproto.EntryKind_ENTRY_KIND_USAGE
	case KindTransferOut:
		return tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT
	case KindTransferIn:
		return tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN
	case KindStarterGrant:
		return tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT
	default:
		panic("entry: Kind.Proto() on unknown kind")
	}
}

// KindFromProto converts a wire-format enum to the Go-typed Kind.
// Returns false for ENTRY_KIND_UNSPECIFIED and any unknown value, so
// callers can reject malformed remote entries cleanly.
func KindFromProto(p tbproto.EntryKind) (Kind, bool) {
	switch p {
	case tbproto.EntryKind_ENTRY_KIND_USAGE:
		return KindUsage, true
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:
		return KindTransferOut, true
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN:
		return KindTransferIn, true
	case tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT:
		return KindStarterGrant, true
	default:
		return 0, false
	}
}
