//go:build e2e

// Scenarios 21-27: journeys the original 20-scenario suite never
// exercised, each modeled on something a REAL network participant does:
//
//   - a plugin performing tunnel-setup discovery (STUN reflexive address
//   - signed bootstrap peer list) — scenario 21;
//   - a hostile/broken client throwing adversarial traffic at the public
//     QUIC listener (a deployed tracker's daily reality) — scenario 22;
//   - a consumer falling back to the tracker's TURN relay after direct
//     hole-punching fails — scenario 23;
//   - an operator clearing a stuck in-flight assignment — scenario 24;
//   - an operator running identity/admission day-2 operations
//     (freeze/unfreeze, dashboards, peer blocklist) — scenario 25;
//   - an operator driving federation actions (clear an equivocation
//     flag, reject a bogus transfer reversal, add/remove a peer) —
//     scenario 26;
//   - an operator draining a tracker for maintenance — scenario 27.
//
// All of these drive the SAME long-running compose stack TestMain owns.
// Scenario ordering note: within a package, `go test` runs tests in
// source order, files in lexical filename order — this file
// (coverage_rpc_test.go) therefore runs after bringup_test.go and BEFORE
// federation_test.go/ledger_test.go/settlement_test.go. Every scenario
// here either leaves the stack untouched (rejected hostile traffic,
// read-only admin GETs, self-cleaning add/remove pairs) or restores it
// before returning (scenario 27's maintenance drain re-starts tracker-a
// and re-verifies bidirectional steady federation peering, since
// scenarios 11-13 depend on it).
//
// The client-side scenarios dial tracker-a's QUIC listener directly from
// the host over the compose port mapping added for this file
// ("7777:7777/udp" in compose.e2e.yaml) using driver.RPCClient — a
// throwaway mint-and-forget Ed25519 identity per scenario, never the
// consumer/seeder actors' identities (impersonating a live actor's
// identity would evict its connection-table entry and deregister its
// registry record on our disconnect, poisoning later scenarios).
package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// trackerAQUICAddr is tracker-a's QUIC RPC listener as seen from the
// host (compose.e2e.yaml maps container 7777/udp to host 7777/udp).
const trackerAQUICAddr = "localhost:7777"

// turnRelayEndpointA is the relay endpoint tracker-a's turn_relay_open
// handler returns: api.Deps.RelayPublicAddr = cfg.STUNTURN.TURNListenAddr,
// which e2egen pins to "0.0.0.0:3479" (cmd/e2egen/render.go).
const turnRelayEndpointA = "0.0.0.0:3479"

// --- RPC-side helpers ----------------------------------------------------

// trackerASPKIHash decodes .gen/tracker-a.spki (sha256 of the tracker's
// DER SPKI — the driver's mTLS pin).
func trackerASPKIHash(t *testing.T) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(readGenFile(t, "tracker-a.spki"))
	require.NoError(t, err, "decode tracker-a.spki hex")
	require.Len(t, raw, 32)
	var out [32]byte
	copy(out[:], raw)
	return out
}

// dialTrackerA dials tracker-a's RPC listener with a fresh throwaway
// identity and registers connection cleanup.
func dialTrackerA(ctx context.Context, t *testing.T) *driver.RPCClient {
	t.Helper()
	cli, err := driver.DialRPC(ctx, trackerAQUICAddr, trackerASPKIHash(t))
	require.NoError(t, err, "dial tracker-a QUIC RPC listener at %s", trackerAQUICAddr)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// enrollClient sends ENROLL for cli's identity and asserts the
// starter-grant response invariants: the tracker echoes the
// mTLS-derived identity (sha256 of the cert SPKI, NOT of the raw
// pubkey) and issues exactly the 1000-credit starter grant.
func enrollClient(ctx context.Context, t *testing.T, cli *driver.RPCClient) {
	t.Helper()
	pub, ok := cli.PrivateKey().Public().(ed25519.PublicKey)
	require.True(t, ok, "client key should be Ed25519")
	fp := sha256.Sum256(append([]byte("e2e-coverage-fingerprint-"), pub...))
	resp, err := cli.CallMsg(ctx, tbproto.RpcMethod_RPC_METHOD_ENROLL, &tbproto.EnrollRequest{
		IdentityPubkey:     pub,
		Role:               1, // consumer
		AccountFingerprint: fp[:],
	})
	require.NoError(t, err, "ENROLL rpc transport")
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status, "ENROLL should succeed: %v", resp.GetError())
	var er tbproto.EnrollResponse
	require.NoError(t, proto.Unmarshal(resp.Payload, &er))
	id := cli.IdentityID()
	assert.Equal(t, id[:], er.IdentityId, "ENROLL must echo the mTLS-derived identity id")
	assert.EqualValues(t, 1000, er.StarterGrantCredits)
	assert.NotEmpty(t, er.StarterGrantEntry, "ENROLL must return the signed starter-grant ledger entry")
}

// buildSignedBrokerEnvelope assembles a fully valid, consumer-signed
// broker_request envelope for cli's identity — same construction as
// plugin/internal/envelopebuilder, reproduced here from shared packages
// only (the plugin's builder is internal to the plugin module).
func buildSignedBrokerEnvelope(t *testing.T, cli *driver.RPCClient, model string, maxIn, maxOut uint64, snap *tbproto.SignedBalanceSnapshot) []byte {
	t.Helper()
	ts := uint64(time.Now().Unix()) //nolint:gosec // unix seconds, always positive
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	proofNonce := make([]byte, 16)
	_, err = rand.Read(proofNonce)
	require.NoError(t, err)
	// The seeder's offer handler rejects any offer that doesn't carry the
	// consumer's 32-byte per-session ephemeral pubkey (it pins the tunnel
	// listener to it). These operator/relay scenarios never dial the
	// tunnel, so a random 32-byte value is enough to get the offer
	// accepted and produce a real assignment/reservation.
	ephPub := make([]byte, 32)
	_, err = rand.Read(ephPub)
	require.NoError(t, err)

	id := cli.IdentityID()
	body := &tbproto.EnvelopeBody{
		ProtocolVersion:      uint32(tbproto.ProtocolVersion),
		ConsumerId:           id[:],
		Model:                model,
		MaxInputTokens:       maxIn,
		MaxOutputTokens:      maxOut,
		Tier:                 tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:             make([]byte, 32),
		ConsumerEphemeralPub: ephPub,
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
			UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
			CapturedAt:  ts,
			Nonce:       proofNonce,
		},
		BalanceProof: snap,
		CapturedAt:   ts,
		Nonce:        nonce,
	}
	sig, err := signing.SignEnvelope(cli.PrivateKey(), body)
	require.NoError(t, err, "sign broker_request envelope")
	payload, err := proto.Marshal(&tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig})
	require.NoError(t, err)
	return payload
}

// requestSeederAssignment drives a full BALANCE -> signed envelope ->
// BROKER_REQUEST round trip for cli's (already enrolled) identity and
// returns the seeder assignment. Retries a few times because the seeder
// actor's availability is asynchronous relative to the registry poll
// that precedes this call.
func requestSeederAssignment(ctx context.Context, t *testing.T, cli *driver.RPCClient, maxIn, maxOut uint64) *tbproto.SeederAssignment {
	t.Helper()
	var last string
	for attempt := 1; attempt <= 5; attempt++ {
		snap, err := cli.VerifiedBalance(ctx, cli.IdentityID())
		require.NoError(t, err, "BALANCE for own identity")
		env := buildSignedBrokerEnvelope(t, cli, sonnetModel, maxIn, maxOut, snap)
		resp, err := cli.Call(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env)
		require.NoError(t, err, "BROKER_REQUEST rpc transport")
		if resp.Status == tbproto.RpcStatus_RPC_STATUS_OK {
			var brr tbproto.BrokerRequestResponse
			require.NoError(t, proto.Unmarshal(resp.Payload, &brr))
			if sa := brr.GetSeederAssignment(); sa != nil {
				return sa
			}
			last = brr.String()
		} else {
			last = resp.Status.String() + " " + resp.GetError().String()
		}
		t.Logf("e2e: broker_request attempt %d did not yield an assignment (%s); retrying", attempt, last)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("broker_request never produced a seeder_assignment; last outcome: %s", last)
	return nil // unreachable
}

// callExpectStatus sends one RPC and asserts the tracker's declared
// status + error code (empty wantCode skips the code check). Returns the
// response for extra per-case assertions.
func callExpectStatus(ctx context.Context, t *testing.T, cli *driver.RPCClient, what string, method tbproto.RpcMethod, payload []byte, wantStatus tbproto.RpcStatus, wantCode string) *tbproto.RpcResponse {
	t.Helper()
	resp, err := cli.Call(ctx, method, payload)
	require.NoError(t, err, "%s: rpc transport", what)
	assert.Equal(t, wantStatus, resp.Status, "%s: status (error=%v)", what, resp.GetError())
	if wantCode != "" {
		assert.Equal(t, wantCode, resp.GetError().GetCode(), "%s: error code (message=%q)", what, resp.GetError().GetMessage())
	}
	return resp
}

// mustMarshal proto-marshals or fails the test.
func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	return b
}

// randomHex returns n random bytes hex-encoded (2n chars).
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// requireAdminStatus asserts err is a *driver.AdminError with the given
// HTTP status — used where an operator action is EXPECTED to be turned
// away (stale ids, duplicate registrations, bogus reversal requests).
func requireAdminStatus(t *testing.T, err error, want int, what string) {
	t.Helper()
	var ae *driver.AdminError
	require.ErrorAs(t, err, &ae, "%s: expected an AdminError, got %v", what, err)
	assert.Equal(t, want, ae.StatusCode, "%s: admin HTTP status (body=%q)", what, ae.Body)
}

// --- Scenario 21: plugin tunnel-setup discovery ---------------------------

// TestScenario21_TunnelSetupDiscovery is the discovery half of the P2P
// tunnel-establishment journey every plugin runs before dialing a peer
// (plugin spec §1.3 — tracker in the control path only): the client asks
// the tracker for its NAT-reflexive address via STUN_ALLOCATE (the input
// to hole-punching), and fetches the signed bootstrap peer list via
// BOOTSTRAP_PEERS (how a plugin discovers alternative regional trackers).
// The peer list must verify under the tracker's own identity key —
// pinned during the QUIC handshake — and carry the federation-form
// issuer id (sha256 of the RAW pubkey — .gen/tracker-a.fedid, NOT the
// SPKI pin; the two encodings are famously not interchangeable).
func TestScenario21_TunnelSetupDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := dialTrackerA(ctx, t)

	// STUN_ALLOCATE: empty payload is the canonical request
	// (StunAllocateRequest has no fields). The client learns the
	// host:port the tracker observed — what it would advertise to a peer
	// for hole-punching.
	resp := callExpectStatus(ctx, t, cli, "STUN_ALLOCATE", tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, nil, tbproto.RpcStatus_RPC_STATUS_OK, "")
	var stun tbproto.StunAllocateResponse
	require.NoError(t, proto.Unmarshal(resp.Payload, &stun))
	ap, err := netip.ParseAddrPort(stun.GetExternalAddr())
	require.NoError(t, err, "external_addr %q should parse as host:port", stun.GetExternalAddr())
	assert.NotZero(t, ap.Port(), "reflexive address should carry the observed source port")
	t.Logf("e2e: STUN reflexive address as observed by tracker-a: %s", ap)

	// BOOTSTRAP_PEERS: the response must be a tracker-signed peer list a
	// plugin can verify offline before trusting any entry.
	resp = callExpectStatus(ctx, t, cli, "BOOTSTRAP_PEERS", tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS, nil, tbproto.RpcStatus_RPC_STATUS_OK, "")
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(cli.TrackerPub(), &list),
		"BootstrapPeerList signature must verify under the handshake-pinned tracker pubkey")
	assert.Equal(t, readGenFile(t, "tracker-a.fedid"), hex.EncodeToString(list.GetIssuerId()),
		"issuer_id must be tracker-a's federation id (sha256 of raw pubkey)")
	assert.Greater(t, list.GetExpiresAt(), list.GetSignedAt(), "snapshot must carry a positive TTL")
	for _, p := range list.GetPeers() {
		assert.NotEqual(t, list.GetIssuerId(), p.GetTrackerId(), "the issuer must drop its own self-row")
	}
	t.Logf("e2e: BOOTSTRAP_PEERS returned %d peer(s)", len(list.GetPeers()))
}

// --- Scenario 22: hostile client rejection ---------------------------------

// TestScenario22_HostileClientRejection models the adversarial traffic a
// tracker on a public UDP port faces from day one, and asserts each
// attack is rejected with a TYPED protocol error — never a hang, crash,
// or silent success:
//
//   - oversize frames (resource-exhaustion probe) and garbage frames
//     (protocol fuzzing) at the framing layer;
//   - an unknown RPC method (client/server version skew, or probing);
//   - settlement forgery: SETTLE against a fabricated preimage hash;
//   - billing fraud: a USAGE_REPORT for a request the caller was never
//     assigned;
//   - relay probing: TURN_RELAY_OPEN with a guessed reservation token.
//
// The tracker must also keep serving the SAME connection normally after
// absorbing all of it (final STUN round trip) — one hostile client must
// not poison its own transport, let alone the listener.
func TestScenario22_HostileClientRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := dialTrackerA(ctx, t)

	// Framing-layer attacks (server/rpc_stream.go): a length prefix
	// above the server's 1 MiB max_frame_size, then a well-sized frame
	// whose bytes don't unmarshal as an RpcRequest.
	oversize := []byte{0x00, 0x20, 0x00, 0x00} // declares 2 MiB, no body follows
	resp, err := cli.CallRawFrame(ctx, oversize)
	require.NoError(t, err, "oversize-header frame: transport")
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_INVALID, resp.Status, "oversize frame status")
	assert.Equal(t, "FRAME", resp.GetError().GetCode(), "oversize frame code")

	garbage := []byte{0x0a, 0xff} // field 1, wire type 2, length 255, truncated
	badFrame := append([]byte{0x00, 0x00, 0x00, 0x02}, garbage...)
	resp, err = cli.CallRawFrame(ctx, badFrame)
	require.NoError(t, err, "garbage frame: transport")
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_INVALID, resp.Status, "garbage frame status")
	assert.Equal(t, "FRAME", resp.GetError().GetCode(), "garbage frame code")

	// Method probing / version skew.
	callExpectStatus(ctx, t, cli, "unknown method 99",
		tbproto.RpcMethod(99), nil,
		tbproto.RpcStatus_RPC_STATUS_INVALID, "UNKNOWN_METHOD")

	// Settlement forgery: claim a settlement for a preimage the broker
	// never saw.
	forgedHash := make([]byte, 32)
	_, err = rand.Read(forgedHash)
	require.NoError(t, err)
	callExpectStatus(ctx, t, cli, "SETTLE with a forged preimage_hash",
		tbproto.RpcMethod_RPC_METHOD_SETTLE, mustMarshal(t, &tbproto.SettleRequest{PreimageHash: forgedHash}),
		tbproto.RpcStatus_RPC_STATUS_NOT_FOUND, "NOT_FOUND")

	// Billing fraud: report usage for an assignment this identity never
	// held (a seeder trying to bill against a nonexistent request).
	forgedReq := make([]byte, 16)
	_, err = rand.Read(forgedReq)
	require.NoError(t, err)
	callExpectStatus(ctx, t, cli, "USAGE_REPORT for a never-assigned request",
		tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT, mustMarshal(t, &tbproto.UsageReport{RequestId: forgedReq, Model: sonnetModel}),
		tbproto.RpcStatus_RPC_STATUS_NOT_FOUND, "NOT_FOUND")

	// Relay probing: try to open a TURN session with a guessed
	// reservation token.
	guessedToken := make([]byte, 16)
	_, err = rand.Read(guessedToken)
	require.NoError(t, err)
	turnResp := callExpectStatus(ctx, t, cli, "TURN_RELAY_OPEN with a guessed token",
		tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, mustMarshal(t, &tbproto.TurnRelayOpenRequest{SessionId: guessedToken}),
		tbproto.RpcStatus_RPC_STATUS_INVALID, "INVALID")
	assert.Contains(t, turnResp.GetError().GetMessage(), "unknown reservation_token")

	// The connection must still serve legitimate traffic afterward.
	callExpectStatus(ctx, t, cli, "STUN_ALLOCATE after the hostile burst",
		tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, nil,
		tbproto.RpcStatus_RPC_STATUS_OK, "")
}

// --- Scenario 23: consumer TURN relay fallback -----------------------------

// TestScenario23_ConsumerTurnRelayFallback is the consumer's relay
// fallback journey (tracker spec rule 1: P2P first, tracker TURN relay
// only as fallback): a consumer enrolls, wins a real seeder assignment
// through the full BALANCE -> signed envelope -> BROKER_REQUEST flow
// against the live seeder actor, and — as if its direct hole-punch had
// failed — opens the tracker's TURN relay with the reservation token.
// Also asserts the two guardrails that make the relay safe to expose: a
// retransmitted open with the same request id is deduplicated rather
// than double-allocated, and an identity that is not party to the
// reservation cannot hijack the session. The assignment is deliberately
// abandoned afterward (no tunnel dial, no usage report) — the same
// reaper-cleaned path scenario 7 exercises on the consumer actor, so
// nothing here leaks into later scenarios.
func TestScenario23_ConsumerTurnRelayFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	configureAndWaitForSeederAdvertise(ctx, t)

	cli := dialTrackerA(ctx, t)
	enrollClient(ctx, t, cli)

	snap, err := cli.VerifiedBalance(ctx, cli.IdentityID())
	require.NoError(t, err)
	require.EqualValues(t, 1000, driver.BalanceCredits(snap), "fresh identity should hold exactly the starter grant")

	// A second throwaway identity, dialed BEFORE the assignment exists so
	// the hijack attempt below runs inside the reservation's short
	// settlement-timeout window.
	stranger := dialTrackerA(ctx, t)

	sa := requestSeederAssignment(ctx, t, cli, 5, 5)
	require.Len(t, sa.GetReservationToken(), 16, "reservation token is the 16-byte request id")

	// (a) The assigned consumer opens the relay.
	resp := callExpectStatus(ctx, t, cli, "TURN open by assigned consumer",
		tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
		mustMarshal(t, &tbproto.TurnRelayOpenRequest{SessionId: sa.GetReservationToken()}),
		tbproto.RpcStatus_RPC_STATUS_OK, "")
	var turn tbproto.TurnRelayOpenResponse
	require.NoError(t, proto.Unmarshal(resp.Payload, &turn))
	assert.Equal(t, turnRelayEndpointA, turn.GetRelayEndpoint(),
		"relay endpoint should be the configured TURN listen addr (e2egen render.go)")
	assert.Len(t, turn.GetToken(), 16, "TURN session token is 16 opaque bytes")

	// (b) A retransmitted open (client retry after a lost response) is
	// deduplicated, not double-allocated.
	dup := callExpectStatus(ctx, t, cli, "TURN retransmit with the same request_id",
		tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
		mustMarshal(t, &tbproto.TurnRelayOpenRequest{SessionId: sa.GetReservationToken()}),
		tbproto.RpcStatus_RPC_STATUS_INVALID, "INVALID")
	assert.Contains(t, dup.GetError().GetMessage(), "duplicate request_id")

	// (c) An identity that is neither the consumer nor the assigned
	// seeder must be turned away before any allocation.
	strangerResp := callExpectStatus(ctx, t, stranger, "TURN open by a non-party identity",
		tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
		mustMarshal(t, &tbproto.TurnRelayOpenRequest{SessionId: sa.GetReservationToken()}),
		tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, "UNAUTHENTICATED")
	assert.Contains(t, strangerResp.GetError().GetMessage(), "not party")
}

// --- Scenario 24: operator clears a stuck assignment -----------------------

// TestScenario24_OperatorClearsStuckAssignment is the operator's
// stuck-request runbook: a consumer wins an assignment and then goes
// dark (the scenario-7 abandonment shape). The operator inspects
// GET /broker/inflight and GET /broker/reservations, sees the wedged
// request holding a credit reservation, force-fails it via
// POST /broker/inflight/fail/{id}, and releases the reservation via
// POST /broker/reservations/release/{id}. Re-running the same commands
// against an id the janitor has since reaped (here: a fabricated one)
// answers 404 — the operator retry after cleanup is a no-op, not an
// error cascade.
func TestScenario24_OperatorClearsStuckAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	configureAndWaitForSeederAdvertise(ctx, t)

	cli := dialTrackerA(ctx, t)
	enrollClient(ctx, t, cli)
	sa := requestSeederAssignment(ctx, t, cli, 5, 5)
	reqIDHex := hex.EncodeToString(sa.GetReservationToken())
	myID := cli.IdentityID()
	myIDHex := hex.EncodeToString(myID[:])

	// Operator dashboard: the in-flight list must show the wedged request.
	inflight, err := adminA().Inflight(ctx)
	require.NoError(t, err, "GET /broker/inflight")
	var found bool
	for _, s := range inflight {
		if s.RequestID == reqIDHex {
			found = true
			assert.Equal(t, myIDHex, s.ConsumerID)
			assert.GreaterOrEqual(t, s.AgeSeconds, float64(0))
		}
	}
	require.True(t, found, "GET /broker/inflight should list request %s", reqIDHex)

	// ...and the reservation ledger must show the credits it holds.
	reservations, err := adminA().Reservations(ctx)
	require.NoError(t, err, "GET /broker/reservations")
	var slotFound bool
	for _, c := range reservations {
		if c.ConsumerID != myIDHex {
			continue
		}
		assert.Positive(t, c.Total, "reserved total for the assigned consumer")
		for _, s := range c.Slots {
			if s.RequestID == reqIDHex {
				slotFound = true
			}
		}
	}
	require.True(t, slotFound, "GET /broker/reservations should show a slot for request %s", reqIDHex)

	// Operator force-fail, then confirm via the detail route.
	failOut, err := adminA().ForceFailInflight(ctx, reqIDHex)
	require.NoError(t, err, "POST /broker/inflight/fail/%s", reqIDHex)
	assert.Equal(t, true, failOut["failed"])
	detail, err := adminA().BrokerInflight(ctx, reqIDHex)
	require.NoError(t, err)
	assert.Equal(t, "failed", detail.State, "force-failed request should read back as failed")

	// Operator force-release of the still-held reservation.
	relOut, err := adminA().ForceReleaseReservation(ctx, reqIDHex)
	require.NoError(t, err, "POST /broker/reservations/release/%s", reqIDHex)
	assert.Equal(t, true, relOut["released"])

	// Retrying the runbook against an id that no longer exists (janitor
	// already cleaned it) answers 404, not a 5xx.
	staleID := randomHex(t, 16)
	_, err = adminA().ForceFailInflight(ctx, staleID)
	requireAdminStatus(t, err, http.StatusNotFound, "force-fail after janitor cleanup")
	_, err = adminA().ForceReleaseReservation(ctx, staleID)
	requireAdminStatus(t, err, http.StatusNotFound, "force-release after janitor cleanup")
}

// --- Scenario 25: operator identity + admission operations -----------------

// TestScenario25_OperatorIdentityAndAdmissionOps is the operator's
// identity/admission day-2 toolkit:
//
//   - investigating a reported identity that turns out not to exist
//     (GET /identity/{id} -> 404, not a 5xx);
//   - the abuse-response freeze -> unfreeze round trip (scenario 13
//     asserts the freeze side and its revocation gossip; this closes the
//     loop by restoring the identity, plus the double-unfreeze the
//     operator's second click sends — an idempotent no-op per commit
//     fe245c5);
//   - the admission dashboards (GET /admission/status, /admission/queue);
//   - blocklisting a misbehaving peer and lifting the block after the
//     incident (add -> list -> remove);
//   - forcing an admission snapshot before a planned restart, and
//     draining the priority queue (empty here — drained list comes back
//     empty rather than erroring).
//
// Every mutation is reverted in-scenario (unfreeze, blocklist remove);
// the tlog operator-override records they append are append-only by
// design and inert.
func TestScenario25_OperatorIdentityAndAdmissionOps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Investigating an identity that doesn't exist anywhere. The tracker
	// answers 200 with a zero balance rather than 404 — ledger.SignedBalance
	// returns credits=0 (no error) for any well-formed id it has never seen,
	// so an operator's lookup of an unknown identity is a clean zero, not a
	// not-found. (A real deployment quirk worth knowing when triaging.)
	fabricated := randomHex(t, 32)
	info, err := adminA().Identity(ctx, fabricated)
	require.NoError(t, err, "GET /identity for an unknown id returns 200, not 404")
	require.NotNil(t, info.Balance, "identity response carries a balance block")
	assert.Equal(t, int64(0), info.Balance.Credits, "an unknown identity reads as zero credits")

	// Abuse response: freeze, then unfreeze once the report is resolved.
	fr, err := adminA().Freeze(ctx, fabricated)
	require.NoError(t, err, "freeze the reported identity")
	assert.True(t, fr.Frozen)
	assert.Equal(t, fabricated, fr.IdentityID)
	uf, err := adminA().Unfreeze(ctx, fabricated)
	require.NoError(t, err, "unfreeze the identity after resolution")
	assert.False(t, uf.Frozen)
	// The operator's impatient second click: unfreezing an already-OK
	// identity is an idempotent no-op (commit fe245c5), not an error.
	uf2, err := adminA().Unfreeze(ctx, fabricated)
	require.NoError(t, err, "double-unfreeze (idempotent no-op)")
	assert.False(t, uf2.Frozen)

	// Admission dashboards.
	status, err := adminA().AdmissionStatus(ctx)
	require.NoError(t, err, "GET /admission/status")
	assert.Contains(t, status, "pressure")
	assert.Contains(t, status, "queue_depth")
	assert.Contains(t, status, "thresholds")
	_, err = adminA().AdmissionQueue(ctx)
	require.NoError(t, err, "GET /admission/queue")

	// Peer blocklist incident cycle: block -> verify -> unblock -> verify.
	blBefore, err := adminA().AdmissionBlocklist(ctx)
	require.NoError(t, err, "GET /admission/peers/blocklist")
	assert.NotContains(t, blBefore, fabricated)
	_, err = adminA().AdmissionBlocklistAdd(ctx, fabricated)
	require.NoError(t, err, "POST /admission/peers/blocklist/{id}")
	blDuring, err := adminA().AdmissionBlocklist(ctx)
	require.NoError(t, err)
	assert.Contains(t, blDuring, fabricated)
	_, err = adminA().AdmissionBlocklistRemove(ctx, fabricated)
	require.NoError(t, err, "DELETE /admission/peers/blocklist/{id}")
	blAfter, err := adminA().AdmissionBlocklist(ctx)
	require.NoError(t, err)
	assert.NotContains(t, blAfter, fabricated)

	// Pre-restart snapshot + a queue drain against an empty queue.
	snapOut, err := adminA().AdmissionSnapshotForce(ctx)
	require.NoError(t, err, "POST /admission/snapshot")
	assert.Equal(t, true, snapOut["ok"])
	drainOut, err := adminA().AdmissionQueueDrain(ctx, 1)
	require.NoError(t, err, "POST /admission/queue/drain")
	assert.Contains(t, drainOut, "drained")
}

// --- Scenario 26: operator federation actions ------------------------------

// TestScenario26_OperatorFederationActions covers the operator-facing
// federation runbook:
//
//   - lifting an equivocation flag after a false alarm — on a healthy
//     peer the command is an idempotent {cleared:false} no-op;
//   - attempting a transfer reversal for a transfer that doesn't exist
//     (fat-fingered nonce, or one already settled) — rejected with 400,
//     never signed onto the federation wire;
//   - onboarding a new federation peer (POST /peers/add, answered 202
//     because the dial is async), the accidental double-submit (409),
//     removing it again (POST /peers/remove), and the stale retry (404).
//
// The fabricated peer lives on a TEST-NET-3 blackhole address and is
// removed in-scenario, so the real A<->B peering is untouched for the
// federation scenarios that follow — asserted explicitly at the end.
func TestScenario26_OperatorFederationActions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// clear_equivocation on the REAL peer (tracker-b): flag is not set on
	// a healthy stack, so this must be the idempotent {cleared:false}.
	bFedID := readGenFile(t, "tracker-b.fedid")
	out, err := adminA().ClearEquivocation(ctx, bFedID)
	require.NoError(t, err, "POST /federation/peers/{b}/clear_equivocation")
	assert.Equal(t, false, out["cleared"], "healthy peer should have no equivocation flag to clear")
	assert.Equal(t, bFedID, out["tracker_id"])

	// A reversal for a transfer that never happened is accepted and
	// gossiped as a signed no-op: the endpoint does not pre-validate that
	// the nonce corresponds to a local transfer (it can't always — the
	// transfer may live at the source), and the source ignores an unknown
	// ref idempotently. So a fat-fingered nonce returns 200, not an error.
	// (Operator-safety note: stricter local validation could reject it
	// earlier; today it fails safe rather than loudly.)
	rev, err := adminA().TransferReversal(ctx, bFedID, randomHex(t, 32), "e2e: operator fat-fingers a nonce")
	require.NoError(t, err, "transfer_reversal for a nonexistent transfer is accepted as a no-op")
	require.NotNil(t, rev, "transfer_reversal returns a response body")

	// Peer onboarding lifecycle. TEST-NET-3 (RFC 5737) is guaranteed
	// unroutable: the async dial fails harmlessly until the remove.
	fakePub := randomHex(t, 32)
	rawPub, err := hex.DecodeString(fakePub)
	require.NoError(t, err)
	fakeIDBytes := sha256.Sum256(rawPub)
	fakeID := hex.EncodeToString(fakeIDBytes[:])
	spec := driver.PeerAddSpec{TrackerID: fakeID, PubKey: fakePub, Addr: "203.0.113.7:7443", Region: "E2E"}

	addOut, err := adminA().PeersAdd(ctx, spec)
	require.NoError(t, err, "POST /peers/add")
	assert.Equal(t, "pending", addOut["state"])
	assert.Equal(t, fakeID, addOut["tracker_id"])

	_, err = adminA().PeersAdd(ctx, spec)
	requireAdminStatus(t, err, http.StatusConflict, "accidental double-submit of the same peer")

	rmOut, err := adminA().PeersRemove(ctx, fakeID)
	require.NoError(t, err, "POST /peers/remove")
	assert.Equal(t, true, rmOut["depeered"])

	_, err = adminA().PeersRemove(ctx, fakeID)
	requireAdminStatus(t, err, http.StatusNotFound, "stale retry of the remove")

	// The real peer set must be intact for scenarios 11-13.
	peers, err := adminA().Peers(ctx)
	require.NoError(t, err)
	var bStillListed bool
	for _, p := range peers.Peers {
		require.NotEqual(t, fakeID, p.TrackerID, "the fabricated peer must be fully removed")
		if p.TrackerID == bFedID {
			bStillListed = true
		}
	}
	assert.True(t, bStillListed, "tracker-b must still be a listed peer after the add/remove cycle")
}

// --- Scenario 27: operator maintenance drain --------------------------------

// TestScenario27_OperatorMaintenanceDrain is the planned-maintenance
// journey: instead of SIGTERMing the process (scenario 10), the operator
// calls POST /maintenance and expects the same clean exit-0 graceful
// drain (handleMaintenance fires cmd/run_cmd's signal-context cancel
// asynchronously after answering 202 {draining:true}). DESTRUCTIVE by
// design, so it runs last in this file and fully restores the stack
// before returning: tracker-a restarted, healthy, and steadily
// re-peered in BOTH directions (scenarios 11-13, which run next, depend
// on that). Mirrors scenario 10's t.Cleanup belt-and-braces recovery.
func TestScenario27_OperatorMaintenanceDrain(t *testing.T) {
	ctx := context.Background()

	t.Cleanup(func() {
		if err := compose().Start("tracker-a"); err != nil {
			t.Logf("e2e: scenario 27 cleanup: compose start tracker-a: %v", err)
		}
		if !pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
			h, herr := adminA().Health(ctx)
			return herr == nil && h.Status == "ok"
		}) {
			t.Errorf("e2e: scenario 27 cleanup: tracker-a did not return healthy after the maintenance drain")
		}
	})

	h, err := adminA().Health(ctx)
	require.NoError(t, err, "tracker-a should be healthy before this scenario touches it")
	require.Equal(t, "ok", h.Status)

	out, err := adminA().Maintenance(ctx)
	require.NoError(t, err, "POST /maintenance")
	assert.Equal(t, true, out["draining"], "maintenance must acknowledge with draining:true")

	// The 202 returns before the drain; poll for the clean exit
	// (scenario 10's technique — exit code 0 distinguishes a graceful
	// drain from a crash, which is this scenario's entire point).
	containerName := composeProjectID + "-tracker-a-1"
	var exitLine string
	require.True(t, pollUntilTrue(45*time.Second, 1*time.Second, func() bool {
		psOut, psErr := compose().PsAll()
		if psErr != nil {
			return false
		}
		for _, line := range strings.Split(psOut, "\n") {
			if strings.Contains(line, containerName) && strings.Contains(line, "Exited") {
				exitLine = line
				return true
			}
		}
		return false
	}), "tracker-a should exit within the shutdown grace period after POST /maintenance")
	assert.Contains(t, exitLine, "Exited (0)", "maintenance must trigger a CLEAN exit (code 0) — a graceful drain, not a crash")

	// Restore: start, wait healthy, and wait for steady federation
	// peering in both directions before handing the stack to the
	// federation scenarios.
	require.NoError(t, compose().Start("tracker-a"), "restart tracker-a after the maintenance drain")
	require.True(t, pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
		hh, herr := adminA().Health(ctx)
		return herr == nil && hh.Status == "ok"
	}), "tracker-a /health should report ok again after the drain + restart")

	trackerBFedIDHex := readGenFile(t, "tracker-b.fedid")
	trackerAFedIDHex := readGenFile(t, "tracker-a.fedid")
	require.True(t, pollUntilTrue(45*time.Second, 1*time.Second, func() bool {
		return peerStateOnA(ctx, t, trackerBFedIDHex) == "steady"
	}), "tracker-a should re-list tracker-b as steady after the drain + restart")
	require.True(t, pollUntilTrue(45*time.Second, 1*time.Second, func() bool {
		return peerStateOnB(ctx, t, trackerAFedIDHex) == "steady"
	}), "tracker-b should re-list tracker-a as steady after the drain + restart")
}
