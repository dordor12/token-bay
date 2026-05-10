package trackerclient

import (
	"crypto/ed25519"
	"time"

	"github.com/google/uuid"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/shared/ids"
)

// ConnectionPhase is the supervisor-reported lifecycle state.
type ConnectionPhase int

const (
	PhaseDisconnected ConnectionPhase = iota
	PhaseConnecting
	PhaseConnected
	PhaseClosing
	PhaseClosed
)

func (p ConnectionPhase) String() string {
	switch p {
	case PhaseDisconnected:
		return "disconnected"
	case PhaseConnecting:
		return "connecting"
	case PhaseConnected:
		return "connected"
	case PhaseClosing:
		return "closing"
	case PhaseClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ConnectionState is a snapshot of supervisor state for slash commands.
type ConnectionState struct {
	Phase         ConnectionPhase
	Endpoint      TrackerEndpoint
	PeerID        ids.IdentityID
	ConnectedAt   time.Time
	LastError     error
	NextAttemptAt time.Time
}

// TrackerEndpoint is one entry in the bootstrap list.
type TrackerEndpoint struct {
	Addr         string   // host:port (UDP for QUIC)
	IdentityHash [32]byte // SHA-256 of the tracker's Ed25519 SPKI bytes
	Region       string
}

// Signer is the keypair holder. internal/identity provides the production
// implementation; tests use a fake.
type Signer interface {
	Sign(msg []byte) ([]byte, error)
	PrivateKey() ed25519.PrivateKey
	IdentityID() ids.IdentityID
}

// EnrollRequest is the consumer-side enrollment payload.
type EnrollRequest struct {
	IdentityPubkey     ed25519.PublicKey
	Role               uint32
	AccountFingerprint [32]byte
	Nonce              [16]byte
	Sig                []byte
}

// EnrollResponse is the tracker's reply.
type EnrollResponse struct {
	IdentityID            ids.IdentityID
	StarterGrantCredits   uint64
	StarterGrantEntryBlob []byte
}

// BrokerOutcome enumerates BrokerRequest's possible result shapes, mirroring
// the wire-format BrokerRequestResponse oneof.
type BrokerOutcome int

const (
	BrokerOutcomeUnspecified BrokerOutcome = iota
	BrokerOutcomeAssignment
	BrokerOutcomeNoCapacity
	BrokerOutcomeQueued
	BrokerOutcomeRejected
)

// SeederAssignment is the body of a BrokerOutcomeAssignment result.
type SeederAssignment struct {
	SeederAddr       string
	SeederPubkey     []byte
	ReservationToken []byte
}

// NoCapacityResult is the body of a BrokerOutcomeNoCapacity result.
type NoCapacityResult struct {
	Reason string
}

// QueuedResult is the body of a BrokerOutcomeQueued result.
type QueuedResult struct {
	RequestID    [16]byte
	PositionBand uint8
	EtaBand      uint8
}

// RejectedResult is the body of a BrokerOutcomeRejected result.
type RejectedResult struct {
	Reason      uint8
	RetryAfterS uint32
}

// BrokerResult is the sum-typed return value of BrokerRequest.
type BrokerResult struct {
	Outcome    BrokerOutcome
	Assignment *SeederAssignment
	NoCap      *NoCapacityResult
	Queued     *QueuedResult
	Rejected   *RejectedResult
}

// UsageReport is what the seeder reports after a successful served request.
type UsageReport struct {
	RequestID    uuid.UUID
	InputTokens  uint32
	OutputTokens uint32
	Model        string
	SeederSig    []byte
}

// Advertisement is the seeder's availability + capabilities update.
type Advertisement struct {
	Models     []string
	MaxContext uint32
	Available  bool
	Headroom   float32
	Tiers      uint32
}

// TransferRequest moves credits between regions.
type TransferRequest struct {
	IdentityID ids.IdentityID
	Amount     uint64
	DestRegion string
	Nonce      [16]byte
}

// TransferProof is the source-region tracker's signed proof.
type TransferProof struct {
	SourceChainTipHash [32]byte
	SourceSeq          uint64
	TrackerSig         []byte
}

// RelayHandle describes a TURN-style relay allocation.
type RelayHandle struct {
	Endpoint string
	Token    []byte
}

// Offer is the server-pushed broker offer to a seeder.
type Offer struct {
	ConsumerID      ids.IdentityID
	EnvelopeHash    [32]byte
	Model           string
	MaxInputTokens  uint32
	MaxOutputTokens uint32
}

// OfferDecision is what the seeder returns to the tracker.
type OfferDecision struct {
	Accept          bool
	EphemeralPubkey []byte
	RejectReason    string
}

// SettlementRequest is the tracker's request that the consumer counter-sign
// a finalized ledger entry.
type SettlementRequest struct {
	PreimageHash [32]byte
	PreimageBody []byte
}

// BootstrapPeer is a plugin-friendly view of a single tracker that came
// out of a verified BootstrapPeerList from FetchBootstrapPeers.
type BootstrapPeer struct {
	TrackerID   ids.IdentityID
	Addr        string
	RegionHint  string
	HealthScore float64
	LastSeen    time.Time
}

// Transport, Conn, and Stream are network-seam interfaces. Drivers live
// under internal/transport/.
type (
	Transport = transport.Transport
	Conn      = transport.Conn
	Stream    = transport.Stream
)

// signerAsIdentity adapts the public Signer interface to the
// internal/transport.Identity interface.
//
//nolint:unused // consumed by supervisor in Task 11
type signerAsIdentity struct{ Signer }

//nolint:unused // consumed by supervisor in Task 11
func (s signerAsIdentity) PrivateKey() ed25519.PrivateKey { return s.Signer.PrivateKey() }

//nolint:unused // consumed by supervisor in Task 11
func (s signerAsIdentity) IdentityID() ids.IdentityID { return s.Signer.IdentityID() }
