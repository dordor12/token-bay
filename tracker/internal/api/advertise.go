package api

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// advertiseRegistry is the slice of registry this handler needs.
type advertiseRegistry interface {
	Advertise(id idsLike, caps registry.Capabilities, available bool, headroom float64) error
}

// installAdvertise wires the live advertise handler when
// Deps.Registry satisfies advertiseRegistry; otherwise the Scope-2 stub.
func (r *Router) installAdvertise() handlerFunc {
	if r.deps.Registry == nil {
		return notImpl("advertise")
	}
	reg, ok := r.deps.Registry.(advertiseRegistry)
	if !ok {
		return notImpl("advertise")
	}
	return func(_ context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var ad tbproto.Advertisement
		if err := proto.Unmarshal(payloadBytes, &ad); err != nil {
			return nil, ErrInvalid(fmt.Sprintf("Advertisement: %v", err))
		}
		caps := registry.Capabilities{
			Models:     ad.Models,
			MaxContext: ad.MaxContext,
			Tiers:      tiersFromBitmask(ad.Tiers),
		}
		if err := reg.Advertise(rc.PeerID, caps, ad.Available, float64(ad.Headroom)); err != nil {
			return nil, fmt.Errorf("advertise: %w", err)
		}
		return OkResponse(nil), nil
	}
}

// tiersFromBitmask maps an Advertisement.Tiers bitmask
// (bit 0 = STANDARD, bit 1 = TEE) to the proto enum slice the
// registry stores.
func tiersFromBitmask(b uint32) []tbproto.PrivacyTier {
	out := []tbproto.PrivacyTier{}
	if b&0x1 != 0 {
		out = append(out, tbproto.PrivacyTier_PRIVACY_TIER_STANDARD)
	}
	if b&0x2 != 0 {
		out = append(out, tbproto.PrivacyTier_PRIVACY_TIER_TEE)
	}
	return out
}
