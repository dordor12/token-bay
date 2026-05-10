// Package federation: per-peer unicast Send.
//
// Slice 0's Gossip.Forward broadcasts to all active peers; cross-region
// credit transfer is point-to-point and uses this primitive instead.
package federation

import (
	"context"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// SendToPeer signs an envelope wrapping payload (with the local tracker
// key) and writes it to the named peer's send queue. Returns
// ErrPeerNotConnected if the peer is not in the active map.
//
// The dedupe Mark is not done here (these messages are not gossiped
// onward). The destination's recv path still calls dedupe.Seen on
// receipt, which catches a hostile peer that re-broadcasts a unicast
// frame.
func (f *Federation) SendToPeer(ctx context.Context, peerID ids.TrackerID, kind fed.Kind, payload []byte) error {
	f.mu.Lock()
	pe, ok := f.peers[peerID]
	f.mu.Unlock()
	if !ok || pe == nil {
		return fmt.Errorf("%w: %x", ErrPeerNotConnected, peerID.Bytes())
	}
	idBytes := f.cfg.MyTrackerID.Bytes()
	env, err := SignEnvelope(f.cfg.MyPriv, idBytes[:], kind, payload)
	if err != nil {
		return fmt.Errorf("federation: sign envelope: %w", err)
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return err
	}
	return pe.Send(ctx, frame)
}
