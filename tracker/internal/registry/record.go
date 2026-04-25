package registry

import (
	"net/netip"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

// Capabilities describes what work a seeder can serve.
//
// Models lists the Claude model IDs (e.g. "claude-opus-4-7") the seeder will
// honor. MaxContext is the largest context window the seeder advertises across
// those models. Tiers lists the privacy tiers the seeder offers; if it
// includes proto.PrivacyTier_PRIVACY_TIER_TEE, Attestation must hold the
// enclave attestation bytes.
//
// Per spec §4.1 the registry only stores capabilities; it does not validate
// the attestation bytes — that is the broker / TEE-tier subsystem's job.
type Capabilities struct {
	Models      []string
	MaxContext  uint32
	Tiers       []proto.PrivacyTier
	Attestation []byte
}

// NetCoords are the network coordinates the tracker records for a seeder so
// the consumer can reach it.
//
// ExternalAddr is the seeder's reflexive address as observed by the tracker
// over its long-lived connection (refreshed on heartbeat per spec §5.4).
// LocalCandidates are additional candidate addresses the seeder advertises
// for hole-punching (RFC 5245-style ICE candidates, slimmed down).
type NetCoords struct {
	ExternalAddr    netip.AddrPort
	LocalCandidates []netip.AddrPort
}

// SeederRecord is the in-memory entry the registry keeps per seeder.
//
// All fields mirror spec §4.1. The registry returns value copies of this
// struct; callers cannot mutate the store via a returned record.
type SeederRecord struct {
	IdentityID       ids.IdentityID
	ConnSessionID    uint64
	Capabilities     Capabilities
	Available        bool
	HeadroomEstimate float64
	ReputationScore  float64
	NetCoords        NetCoords
	Load             int
	LastHeartbeat    time.Time
}
