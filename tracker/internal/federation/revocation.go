// Package federation: revocation gossip coordinator.
//
// revocationCoordinator owns:
//   - the outbound emit path: reputation freezes -> signed Revocation
//     -> forward via gossip + persist locally
//   - the inbound apply path: validate -> issuer-sig-verify -> archive
//     (idempotent INSERT OR IGNORE) -> forward onward
//
// Concurrency: stateless; each call is one synchronous flow. Storage
// idempotency comes from the SQLite primary-key INSERT OR IGNORE in
// PeerRevocationArchive.PutPeerRevocation. Gossip-echo suppression
// comes from the slice-0 dedupe core (the dispatcher Seen-checks
// before invoking OnIncoming).
package federation

import (
	"context"
	"crypto/ed25519"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

type revocationCoordinatorCfg struct {
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	Archive     PeerRevocationArchive
	Forward     Forwarder
	PeerPubKey  func(ids.TrackerID) (ed25519.PublicKey, bool)
	Now         func() time.Time
	Metrics     func(name string) // increment a named counter
}

type revocationCoordinator struct {
	cfg revocationCoordinatorCfg
}

func newRevocationCoordinator(cfg revocationCoordinatorCfg) *revocationCoordinator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = func(string) {}
	}
	return &revocationCoordinator{cfg: cfg}
}

// mapReputationReasonToProto translates a reputation-subsystem reason
// string (Signal in ReasonRecord.Kind=="transition") into the wire enum.
// Unknown reputation reasons default to ABUSE: reputation only emits
// freeze-class transitions in v1, all of which are abuse-driven.
func mapReputationReasonToProto(s string) fed.RevocationReason {
	switch s {
	case "freeze_repeat":
		return fed.RevocationReason_REVOCATION_REASON_ABUSE
	case "operator":
		return fed.RevocationReason_REVOCATION_REASON_MANUAL
	default:
		return fed.RevocationReason_REVOCATION_REASON_ABUSE
	}
}

// OnFreeze emits a signed REVOCATION for identity, broadcasts via the
// gossip Forward closure, and persists locally so a later round-trip
// echo from peers is a no-op at the archive layer (and dedupe at the
// envelope layer).
func (rc *revocationCoordinator) OnFreeze(ctx context.Context, identity ids.IdentityID, reason string, revokedAt time.Time) {
	if rc == nil || rc.cfg.Archive == nil {
		if rc != nil {
			rc.cfg.Metrics("revocation_emit_disabled")
		}
		return
	}
	myID := rc.cfg.MyTrackerID.Bytes()
	idArr := identity
	rev := &fed.Revocation{
		TrackerId:  myID[:],
		IdentityId: idArr[:],
		Reason:     mapReputationReasonToProto(reason),
		RevokedAt:  uint64(revokedAt.Unix()), //nolint:gosec // G115 — Unix() ≥ 0 for any post-epoch timestamp
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_canonical")
		return
	}
	rev.TrackerSig = ed25519.Sign(rc.cfg.MyPriv, canonical)
	if err := fed.ValidateRevocation(rev); err != nil {
		rc.cfg.Metrics("revocation_self_shape")
		return
	}
	payload, err := proto.Marshal(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_marshal")
		return
	}

	if err := rc.cfg.Archive.PutPeerRevocation(ctx, storage.PeerRevocation{
		TrackerID:  append([]byte(nil), rev.TrackerId...),
		IdentityID: append([]byte(nil), rev.IdentityId...),
		Reason:     uint32(rev.Reason), //nolint:gosec // G115 — RevocationReason enum values are non-negative
		RevokedAt:  rev.RevokedAt,
		TrackerSig: append([]byte(nil), rev.TrackerSig...),
		ReceivedAt: uint64(rc.cfg.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0
	}); err != nil {
		rc.cfg.Metrics("revocation_archive_self_err")
		// Continue forwarding — peers should still learn even if our
		// own archive write hit a transient error.
	}

	rc.cfg.Forward(ctx, fed.Kind_KIND_REVOCATION, payload)
	rc.cfg.Metrics("revocations_emitted")
}

// OnIncoming validates a received KIND_REVOCATION envelope, verifies the
// issuer's signature against the operator-allowlisted pubkey, archives
// idempotently, and forwards to all active peers via the slice-0 forward
// closure. The dedupe core suppresses round-trip echoes.
func (rc *revocationCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, _ ids.TrackerID) {
	if rc == nil || rc.cfg.Archive == nil {
		if rc != nil {
			rc.cfg.Metrics("revocation_disabled")
		}
		return
	}
	rev := &fed.Revocation{}
	if err := proto.Unmarshal(env.Payload, rev); err != nil {
		rc.cfg.Metrics("revocation_shape")
		return
	}
	if err := fed.ValidateRevocation(rev); err != nil {
		rc.cfg.Metrics("revocation_shape")
		return
	}
	var issuerID ids.TrackerID
	copy(issuerID[:], rev.TrackerId)
	issuerPub, ok := rc.cfg.PeerPubKey(issuerID)
	if !ok {
		rc.cfg.Metrics("revocation_unknown_issuer")
		return
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_canonical")
		return
	}
	if !ed25519.Verify(issuerPub, canonical, rev.TrackerSig) {
		rc.cfg.Metrics("revocation_sig")
		return
	}
	if err := rc.cfg.Archive.PutPeerRevocation(ctx, storage.PeerRevocation{
		TrackerID:  append([]byte(nil), rev.TrackerId...),
		IdentityID: append([]byte(nil), rev.IdentityId...),
		Reason:     uint32(rev.Reason), //nolint:gosec // G115 — RevocationReason enum values are non-negative
		RevokedAt:  rev.RevokedAt,
		TrackerSig: append([]byte(nil), rev.TrackerSig...),
		ReceivedAt: uint64(rc.cfg.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0
	}); err != nil {
		// Cannot vouch for durability — do not forward.
		rc.cfg.Metrics("revocation_archive_err")
		return
	}
	rc.cfg.Forward(ctx, fed.Kind_KIND_REVOCATION, env.Payload)
	rc.cfg.Metrics("revocations_archived")
}
