package federation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// PeerRootArchive is the slice of *storage.Store the federation receive
// path needs. Backed by *ledger/storage.Store in production.
type PeerRootArchive interface {
	PutPeerRoot(ctx context.Context, p storage.PeerRoot) error
	GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error)
}

// Forwarder forwards an already-validated payload to all active peers
// except the source. Implemented by *Gossip; abstracted so rootattest
// can be unit-tested in isolation.
type Forwarder func(ctx context.Context, kind fed.Kind, payload []byte)

// NowFunc is the injection point for the wall-clock used to stamp
// PeerRoot.ReceivedAt. Tests substitute a fake clock.
type NowFunc func() time.Time

// NowFromTime is the production NowFunc.
func NowFromTime() time.Time { return time.Now() }

// RootAttestApplier is the receive-side handler for KIND_ROOT_ATTESTATION
// envelopes. Equivocation handling is provided by RegisterEquivocator.
type RootAttestApplier struct {
	archive PeerRootArchive
	forward Forwarder
	now     NowFunc
	equivoc func(ctx context.Context, incoming *fed.RootAttestation, srcEnv *fed.Envelope)
}

func NewRootAttestApplier(arch PeerRootArchive, fwd Forwarder, now NowFunc) *RootAttestApplier {
	return &RootAttestApplier{archive: arch, forward: fwd, now: now}
}

// RegisterEquivocator wires the equivocation branch. Called once during
// Open after Equivocator is built.
func (r *RootAttestApplier) RegisterEquivocator(fn func(context.Context, *fed.RootAttestation, *fed.Envelope)) {
	r.equivoc = fn
}

// Apply parses and persists the root attestation carried by env. On
// archive-conflict (storage.ErrPeerRootConflict) the equivocation branch
// is invoked instead of forwarding. Other errors are returned to the
// caller (the recvLoop's dispatch increments a metric and drops).
func (r *RootAttestApplier) Apply(ctx context.Context, env *fed.Envelope) error {
	var msg fed.RootAttestation
	if err := proto.Unmarshal(env.Payload, &msg); err != nil {
		return fmt.Errorf("federation: rootattest unmarshal: %w", err)
	}
	if err := fed.ValidateRootAttestation(&msg); err != nil {
		return err
	}
	if !bytes.Equal(msg.TrackerId, env.SenderId) {
		return fmt.Errorf("federation: rootattest tracker_id != envelope.sender_id")
	}
	pr := storage.PeerRoot{
		TrackerID:  append([]byte(nil), msg.TrackerId...),
		Hour:       msg.Hour,
		Root:       append([]byte(nil), msg.MerkleRoot...),
		Sig:        append([]byte(nil), msg.TrackerSig...),
		ReceivedAt: uint64(r.now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0 for any post-epoch timestamp
	}
	switch err := r.archive.PutPeerRoot(ctx, pr); {
	case err == nil:
		r.forward(ctx, fed.Kind_KIND_ROOT_ATTESTATION, env.Payload)
		return nil
	case errors.Is(err, storage.ErrPeerRootConflict):
		if r.equivoc != nil {
			r.equivoc(ctx, &msg, env)
		}
		return ErrEquivocation
	default:
		return fmt.Errorf("federation: archive put: %w", err)
	}
}
