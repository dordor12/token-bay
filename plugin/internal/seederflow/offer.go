package seederflow

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// HandleOffer implements trackerclient.OfferHandler. It applies the
// availability gate (idle policy + activity grace + headroom +
// conformance) and the model-supported check; if both pass, generates a
// fresh ephemeral keypair, registers a reservation under the offer's
// EnvelopeHash, and returns Accept with the ephemeral pubkey.
//
// On reject, the returned error is always nil — RejectReason carries
// the human explanation. Returning an error here causes trackerclient
// to write a generic-decline back, which is less useful for the broker.
func (c *Coordinator) HandleOffer(ctx trackerclient.Ctx, o *trackerclient.Offer) (trackerclient.OfferDecision, error) {
	if err := ctx.Err(); err != nil {
		return trackerclient.OfferDecision{Accept: false, RejectReason: "ctx canceled"}, nil
	}
	now := c.cfg.Clock()

	c.mu.Lock()
	avail := c.availableLocked(now)
	c.mu.Unlock()
	if !avail {
		return trackerclient.OfferDecision{
			Accept:       false,
			RejectReason: rejectReasonForState(c, now),
		}, nil
	}

	if !modelSupported(c.cfg.Models, o.Model) {
		return trackerclient.OfferDecision{
			Accept:       false,
			RejectReason: "model not advertised: " + o.Model,
		}, nil
	}

	pub, priv, err := ed25519.GenerateKey(c.cfg.Rand)
	if err != nil {
		return trackerclient.OfferDecision{
			Accept:       false,
			RejectReason: "ephemeral keygen failed",
		}, nil
	}

	res := &reservation{
		envelopeHash:    o.EnvelopeHash,
		consumerIDHash:  sha256.Sum256(o.ConsumerID[:]),
		model:           o.Model,
		maxInputTokens:  o.MaxInputTokens,
		maxOutputTokens: o.MaxOutputTokens,
		ephemeralPub:    pub,
		ephemeralPriv:   priv,
		registeredAt:    now,
	}
	key := hex.EncodeToString(o.EnvelopeHash[:])
	c.mu.Lock()
	c.reservations[key] = res
	c.mu.Unlock()

	return trackerclient.OfferDecision{
		Accept:          true,
		EphemeralPubkey: pub,
	}, nil
}

// HasReservation reports whether an unconsumed reservation exists for
// envHash. Public for test introspection.
func (c *Coordinator) HasReservation(envHash [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.reservations[hex.EncodeToString(envHash[:])]
	return ok
}

func modelSupported(models []string, want string) bool {
	return slices.Contains(models, want)
}

func rejectReasonForState(c *Coordinator, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conformanceFailed {
		return "bridge conformance failed"
	}
	if c.activityActive {
		return "user is actively using Claude Code"
	}
	if !c.lastActivityAt.IsZero() && now.Sub(c.lastActivityAt) < c.cfg.ActivityGrace {
		return "within activity grace window"
	}
	if !c.lastRateLimitAt.IsZero() && now.Sub(c.lastRateLimitAt) < c.cfg.HeadroomWindow {
		return "recent rate-limit signal — no headroom"
	}
	if !inIdleWindow(c.cfg.IdlePolicy, now) {
		return "outside idle window"
	}
	return "not advertising"
}
