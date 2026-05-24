// Package seederflow is the seeder-side coordinator that turns
// tracker-pushed offers into airtight bridge invocations.
//
// # Position
//
// One *Coordinator per sidecar process owns:
//
//   - the seeder-side OfferHandler implementation (registered on
//     trackerclient.Config). Each accepted offer registers a
//     reservation with a fresh ephemeral keypair returned to the
//     consumer via OfferDecision.
//   - a tunnel.Listener accept loop that receives consumer-dialed
//     QUIC connections and serves each via ccbridge.Bridge wrapped
//     in ssetranslate.Writer.
//   - the availability state machine that gates Advertise calls on
//     idle policy, activity grace, and the recent-rate-limit
//     headroom heuristic (plugin spec §6.3).
//   - the seeder-side ActiveClientChecker the ccbridge.Janitor
//     consults to keep per-client session folders for live peers.
//
// # Tunnel binding
//
// OfferPush carries consumer_ephemeral_pub (32 bytes). HandleOffer
// rejects any offer that omits or malforms this field and bumps the
// Metrics counter "no_ephemeral". On accept, the Coordinator generates
// a fresh seeder ephemeral keypair and calls Acceptor.Bind(seederPriv,
// consumerPub) before returning the OfferDecision. The production
// Acceptor is a RebindingAcceptor that wraps a TunnelListenerFactory
// (which in turn wraps internal/tunnel.Listen). Each Bind tears down
// the previous listener and stands up a fresh one bound to the new
// (seederPriv, consumerPub) pair so QUIC + TLS pinning rejects any
// dialer whose ephemeral keypair differs from the consumer's.
//
// # Spec
//
// Plugin design §6 (entire), §11 (failure modes), §12 (acceptance).
//
// # Out of scope
//
// Consumer fallback (lives in ccproxy + a separate plan), settlement
// counter-signing (separate small task), enroll, federation. This
// package never modifies shared/ or tracker/.
package seederflow
