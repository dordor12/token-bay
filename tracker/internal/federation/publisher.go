package federation

import (
	"context"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// RootSource yields the local tracker's signed Merkle root for hour.
// ok=false means not yet available; the publisher logs and retries on
// the next tick.
type RootSource interface {
	ReadyRoot(ctx context.Context, hour uint64) (root, sig []byte, ok bool, err error)
}

// Publisher emits ROOT_ATTESTATION messages outbound on demand.
type Publisher struct {
	src     RootSource
	forward Forwarder
	myID    ids.TrackerID
}

func NewPublisher(src RootSource, fwd Forwarder, myID ids.TrackerID) *Publisher {
	return &Publisher{src: src, forward: fwd, myID: myID}
}

// PublishHour pulls the (root, sig) for hour from RootSource and
// broadcasts a ROOT_ATTESTATION to all active peers. Returns nil on a
// ready root or a !ok skip; returns the underlying error if the source
// failed.
func (p *Publisher) PublishHour(ctx context.Context, hour uint64) error {
	root, sig, ok, err := p.src.ReadyRoot(ctx, hour)
	if err != nil {
		return fmt.Errorf("federation: RootSource: %w", err)
	}
	if !ok {
		return nil
	}
	idBytes := p.myID.Bytes()
	msg := &fed.RootAttestation{
		TrackerId:  idBytes[:],
		Hour:       hour,
		MerkleRoot: root,
		TrackerSig: sig,
	}
	if err := fed.ValidateRootAttestation(msg); err != nil {
		return err
	}
	payload, _ := proto.Marshal(msg)
	p.forward(ctx, fed.Kind_KIND_ROOT_ATTESTATION, payload)
	return nil
}
