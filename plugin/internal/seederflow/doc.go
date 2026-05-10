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
// # Wire-format compatibility note
//
// shared/proto OfferPush carries the consumer's identity hash but
// not a per-session ephemeral pubkey, and the tunnel package's
// listener requires PeerPin at bind time. Until OfferPush gains a
// consumer-ephemeral-pubkey field, the production listener is
// bound only after an offer is accepted using the consumer's
// identity-derived pin material. The Acceptor abstraction in
// types.go isolates this so tests can exercise the offer→serve
// flow without standing up a real listener.
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
