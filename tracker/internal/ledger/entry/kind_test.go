package entry

import (
	"testing"

	"github.com/stretchr/testify/assert"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestKind_String(t *testing.T) {
	cases := map[Kind]string{
		KindUsage:        "usage",
		KindTransferOut:  "transfer_out",
		KindTransferIn:   "transfer_in",
		KindStarterGrant: "starter_grant",
		Kind(99):         "unknown",
	}
	for k, want := range cases {
		assert.Equal(t, want, k.String())
	}
}

func TestKind_Proto(t *testing.T) {
	cases := map[Kind]tbproto.EntryKind{
		KindUsage:        tbproto.EntryKind_ENTRY_KIND_USAGE,
		KindTransferOut:  tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT,
		KindTransferIn:   tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN,
		KindStarterGrant: tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT,
	}
	for k, want := range cases {
		assert.Equal(t, want, k.Proto(), "kind %v", k)
	}
}

func TestKind_Proto_PanicsOnUnknown(t *testing.T) {
	assert.Panics(t, func() { _ = Kind(99).Proto() })
}

func TestKindFromProto_KnownEnums(t *testing.T) {
	cases := map[tbproto.EntryKind]Kind{
		tbproto.EntryKind_ENTRY_KIND_USAGE:         KindUsage,
		tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:  KindTransferOut,
		tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN:   KindTransferIn,
		tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT: KindStarterGrant,
	}
	for in, want := range cases {
		got, ok := KindFromProto(in)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
}

func TestKindFromProto_RejectsUnspecifiedAndUnknown(t *testing.T) {
	_, ok := KindFromProto(tbproto.EntryKind_ENTRY_KIND_UNSPECIFIED)
	assert.False(t, ok)
	_, ok = KindFromProto(tbproto.EntryKind(99))
	assert.False(t, ok)
}
