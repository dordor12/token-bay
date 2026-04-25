package registry

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

func TestSeederRecord_ZeroValue(t *testing.T) {
	var r SeederRecord
	assert.Equal(t, ids.IdentityID{}, r.IdentityID)
	assert.Equal(t, uint64(0), r.ConnSessionID)
	assert.Equal(t, Capabilities{}, r.Capabilities)
	assert.False(t, r.Available)
	assert.Equal(t, 0.0, r.HeadroomEstimate)
	assert.Equal(t, 0.0, r.ReputationScore)
	assert.Equal(t, NetCoords{}, r.NetCoords)
	assert.Equal(t, 0, r.Load)
	assert.True(t, r.LastHeartbeat.IsZero())
}

func TestSeederRecord_PopulatedFields(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddrPort("203.0.113.7:51820")
	local := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.10:51820"),
		netip.MustParseAddrPort("[fe80::1]:51820"),
	}

	r := SeederRecord{
		IdentityID:    ids.IdentityID{0x01, 0x02, 0x03},
		ConnSessionID: 42,
		Capabilities: Capabilities{
			Models:      []string{"claude-opus-4-7", "claude-sonnet-4-6"},
			MaxContext:  200_000,
			Tiers:       []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
			Attestation: nil,
		},
		Available:        true,
		HeadroomEstimate: 0.75,
		ReputationScore:  0.92,
		NetCoords: NetCoords{
			ExternalAddr:    addr,
			LocalCandidates: local,
		},
		Load:          3,
		LastHeartbeat: now,
	}

	assert.Equal(t, [32]byte{0x01, 0x02, 0x03}, r.IdentityID.Bytes())
	assert.Equal(t, []string{"claude-opus-4-7", "claude-sonnet-4-6"}, r.Capabilities.Models)
	assert.Equal(t, uint32(200_000), r.Capabilities.MaxContext)
	assert.Equal(t, []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD}, r.Capabilities.Tiers)
	assert.Nil(t, r.Capabilities.Attestation)
	assert.True(t, r.Available)
	assert.Equal(t, 0.75, r.HeadroomEstimate)
	assert.Equal(t, 0.92, r.ReputationScore)
	assert.Equal(t, addr, r.NetCoords.ExternalAddr)
	assert.Equal(t, local, r.NetCoords.LocalCandidates)
	assert.Equal(t, 3, r.Load)
	assert.Equal(t, now, r.LastHeartbeat)
}

func TestCapabilities_TEEAttestationOptional(t *testing.T) {
	c := Capabilities{
		Models:      []string{"claude-opus-4-7"},
		MaxContext:  100_000,
		Tiers:       []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_TEE},
		Attestation: []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	assert.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, c.Attestation)
}
