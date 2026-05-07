package broker

import (
	"context"
	"errors"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

var errUnreachable = errors.New("broker: seeder unreachable")

// runOffer performs a single offer round-trip against the given seeder.
// Returns:
//   - (true, ephPub, nil)  on accept
//   - (false, nil, nil)    on reject or timeout
//   - (false, nil, err)    on push setup failure or context cancellation
func runOffer(ctx context.Context, pusher PushService, seederID ids.IdentityID,
	body *tbproto.EnvelopeBody, envHash [32]byte, timeout time.Duration,
) (accept bool, ephemeralPub []byte, err error) {
	push := &tbproto.OfferPush{
		ConsumerId:      body.ConsumerId,
		EnvelopeHash:    envHash[:],
		Model:           body.Model,
		MaxInputTokens:  uint32(body.MaxInputTokens),  //nolint:gosec // G115: truncation acceptable for offer push; seeder ignores the value
		MaxOutputTokens: uint32(body.MaxOutputTokens), //nolint:gosec // G115: same
	}
	ch, ok := pusher.PushOfferTo(seederID, push)
	if !ok {
		return false, nil, errUnreachable
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case dec := <-ch:
		if dec == nil || !dec.Accept {
			return false, nil, nil
		}
		return true, dec.EphemeralPubkey, nil
	case <-timer.C:
		return false, nil, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}
