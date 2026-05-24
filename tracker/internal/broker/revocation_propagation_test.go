package broker_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// dualArchive satisfies both federation.PeerRevocationArchive (the gossip
// inbound writer) and broker.RevocationLookup (the broker pre-check).
// In production both roles are filled by *storage.Store; here we keep an
// in-memory map so the test stays fast.
type dualArchive struct {
	mu   sync.Mutex
	rows map[[64]byte]storage.PeerRevocation
}

func newDualArchive() *dualArchive {
	return &dualArchive{rows: map[[64]byte]storage.PeerRevocation{}}
}

func (d *dualArchive) PutPeerRevocation(_ context.Context, r storage.PeerRevocation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var k [64]byte
	copy(k[0:32], r.TrackerID)
	copy(k[32:64], r.IdentityID)
	if _, ok := d.rows[k]; ok {
		return nil
	}
	d.rows[k] = r
	return nil
}

func (d *dualArchive) GetPeerRevocation(_ context.Context, tr, id []byte) (storage.PeerRevocation, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var k [64]byte
	copy(k[0:32], tr)
	copy(k[32:64], id)
	r, ok := d.rows[k]
	return r, ok, nil
}

func (d *dualArchive) IsIdentityRevoked(_ context.Context, identityID []byte) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.rows {
		if bytes.Equal(r.IdentityID, identityID) {
			return true, nil
		}
	}
	return false, nil
}

// stubPushService satisfies broker.PushService — the cross-region
// revocation test never reaches the offer stage, so the push channels
// stay unused. Returning ok=false ensures any unexpected reach into
// the offer/settlement paths fails loudly.
type stubPushService struct{}

func (stubPushService) PushOfferTo(_ ids.IdentityID, _ *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	return nil, false
}

func (stubPushService) PushSettlementTo(_ ids.IdentityID, _ *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
	return nil, false
}

func TestIntegration_FrozenAtPeer_BrokerRejectsLocally(t *testing.T) {
	t.Parallel()

	// --- Federation pair A <-> B -------------------------------------
	hub := federation.NewInprocHub()

	aPub, aPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	bPub, bPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	aID := ids.TrackerID(sha256.Sum256(aPub))
	bID := ids.TrackerID(sha256.Sum256(bPub))

	trA := federation.NewInprocTransport(hub, "A", aPub, aPriv)
	trB := federation.NewInprocTransport(hub, "B", bPub, bPriv)

	revArchA := newDualArchive()
	revArchB := newDualArchive()

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID,
		MyPriv:      aPriv,
		Peers: []federation.AllowlistedPeer{
			{TrackerID: bID, PubKey: bPub, Addr: "B"},
		},
	}, federation.Deps{
		Transport:         trA,
		RootSrc:           noopRootSrc{},
		Archive:           noopRootArchive{},
		RevocationArchive: revArchA,
		Metrics:           federation.NewMetrics(prometheus.NewRegistry()),
		Logger:            zerolog.Nop(),
		Now:               time.Now,
	})
	require.NoError(t, err)
	defer aFed.Close()

	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      bPriv,
		Peers: []federation.AllowlistedPeer{
			{TrackerID: aID, PubKey: aPub, Addr: "A"},
		},
	}, federation.Deps{
		Transport:         trB,
		RootSrc:           noopRootSrc{},
		Archive:           noopRootArchive{},
		RevocationArchive: revArchB,
		Metrics:           federation.NewMetrics(prometheus.NewRegistry()),
		Logger:            zerolog.Nop(),
		Now:               time.Now,
	})
	require.NoError(t, err)
	defer bFed.Close()

	// Wait until A's view of B is Steady. A is the revocation publisher
	// (aFed.OnFreeze below); gossip.Forward only enumerates peers from
	// the *publisher's* peers map, so waiting on B's view is the wrong
	// side — that side races independently with A's own attachPeerLocked
	// and can leave A.peers[B] empty when the publish fires, silently
	// dropping the revocation. B's recv path is buffered (16-frame conn
	// channel) and drains as soon as B's attach completes within the
	// 2s revocation-archive wait below.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range aFed.Peers() {
			if p.State == federation.PeerStateSteady {
				goto steady
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("A never reached steady state with B")
steady:

	// --- Broker on tracker B ----------------------------------------
	// The revocation pre-check runs before any seeder selection, so an
	// empty registry is sufficient: a clean (non-frozen) identity here
	// would surface "no_eligible_seeder", whereas the frozen identity
	// must short-circuit with ErrIdentityFrozen.
	reg, err := registry.New(8)
	require.NoError(t, err)

	brokerB, err := broker.OpenBroker(brokerCfg(), settlementCfg(), broker.Deps{
		Logger:            zerolog.Nop(),
		Registry:          regAdapter{Registry: reg},
		Ledger:            stubLedger{},
		Admission:         stubAdmission{},
		Pusher:            stubPushService{},
		Pricing:           broker.DefaultPriceTable(),
		TrackerKey:        bPriv,
		RevocationArchive: revArchB,
	}, nil)
	require.NoError(t, err)
	defer brokerB.Close()

	// --- Trigger freeze on A; gossip carries it to B's archive ------
	identity := ids.IdentityID(bytes.Repeat([]byte{0x44}, 32))
	aFed.OnFreeze(context.Background(), identity, "freeze_repeat", time.Unix(1714000100, 0))

	// Wait until B's archive has the revocation (proxy for "broker
	// pre-check will see it"). Reputation §10 acceptance: revocations
	// propagate within 5 minutes; the inproc hub is synchronous, so
	// we expect this within milliseconds.
	aIDBytes := aID.Bytes()
	deadline = time.Now().Add(2 * time.Second)
	archived := false
	for time.Now().Before(deadline) {
		if _, ok, _ := revArchB.GetPeerRevocation(context.Background(), aIDBytes[:], identity[:]); ok {
			archived = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, archived, "B never archived A's revocation")

	// --- broker_request for the frozen identity must return frozen --
	env := envelopeFor(identity)
	_, err = brokerB.Submit(context.Background(), env)
	require.ErrorIs(t, err, broker.ErrIdentityFrozen,
		"B's broker must honour cross-region revocation")
}

// ---------------------------------------------------------------------
// Tiny stubs for the non-revocation broker.Deps slots. Keep them in
// this file so the integration test remains self-contained.
// ---------------------------------------------------------------------

type noopRootSrc struct{}

func (noopRootSrc) ReadyRoot(_ context.Context, _ uint64) ([]byte, []byte, bool, error) {
	return nil, nil, false, nil
}

type noopRootArchive struct{}

func (noopRootArchive) PutPeerRoot(_ context.Context, _ storage.PeerRoot) error { return nil }
func (noopRootArchive) GetPeerRoot(_ context.Context, _ []byte, _ uint64) (storage.PeerRoot, bool, error) {
	return storage.PeerRoot{}, false, nil
}

type stubLedger struct{}

func (stubLedger) Tip(_ context.Context) (uint64, []byte, bool, error) {
	return 0, make([]byte, 32), false, nil
}

func (stubLedger) AppendUsage(_ context.Context, _ ledger.UsageRecord) (*tbproto.Entry, error) {
	return nil, nil
}

type stubAdmission struct{}

func (stubAdmission) PopReadyForBroker(_ time.Time, _ float64) (admission.QueueEntry, bool) {
	return admission.QueueEntry{}, false
}
func (stubAdmission) PressureGauge() float64 { return 0 }
func (stubAdmission) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return admission.Result{Outcome: admission.OutcomeAdmit}
}

// regAdapter wraps a *registry.Registry to add the broker's narrower
// Match/Get/IncLoad/DecLoad slice. *registry.Registry satisfies broker
// directly in production; the adapter exists so compile-time interface
// checks on this test file fail loudly if the broker contract drifts.
type regAdapter struct{ *registry.Registry }

func envelopeFor(consumer ids.IdentityID) *tbproto.EnvelopeSigned {
	return &tbproto.EnvelopeSigned{
		Body: &tbproto.EnvelopeBody{
			ProtocolVersion: 1,
			ConsumerId:      consumer[:],
			Model:           "claude-sonnet-4-6",
			MaxInputTokens:  100,
			MaxOutputTokens: 200,
			Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
			BodyHash:        bytes.Repeat([]byte{0x01}, 32),
			CapturedAt:      1700000000,
			Nonce:           bytes.Repeat([]byte{0x02}, 16),
			BalanceProof: &tbproto.SignedBalanceSnapshot{
				Body: &tbproto.BalanceSnapshotBody{Credits: 1_000_000},
			},
		},
	}
}

func brokerCfg() config.BrokerConfig {
	return config.BrokerConfig{
		HeadroomThreshold: 0.2,
		LoadThreshold:     5,
		ScoreWeights: config.BrokerScoreWeights{
			Reputation: 0.4, Headroom: 0.3, RTT: 0.2, Load: 0.1,
		},
		OfferTimeoutMs:          1500,
		MaxOfferAttempts:        4,
		BrokerRequestRatePerSec: 2.0,
		QueueDrainIntervalMs:    1000,
		InflightTerminalTTLS:    600,
	}
}

func settlementCfg() config.SettlementConfig {
	return config.SettlementConfig{
		TunnelSetupMs:      10000,
		StreamIdleS:        60,
		SettlementTimeoutS: 900,
		ReservationTTLS:    1200,
		StaleTipRetries:    3,
	}
}
