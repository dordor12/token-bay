package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/plugin/internal/tunnel"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// balanceTimeout bounds one Balance RPC inside /request and /balance.
const balanceTimeout = 5 * time.Second

// brokerTimeout bounds the BrokerRequest RPC.
const brokerTimeout = 10 * time.Second

// tunnelBudget bounds dial + send + full SSE read against the assigned
// seeder. Comfortably below the seeder's 60s serveWindow.
const tunnelBudget = 15 * time.Second

// settleTimeout bounds the Settle RPC the SettlementHandler makes.
const settleTimeout = 5 * time.Second

// transferTimeout bounds tracker-B connect + the TransferRequest RPC.
const transferTimeout = 10 * time.Second

// usageAssertionDomain mirrors shared/signing's usage-assertion domain tag;
// parseAssertionRequestID checks it before trusting the preimage layout.
const usageAssertionDomain = "token-bay/usage-assertion:v1"

// enrollSigner wraps the identity signer so envelopebuilder sees
// IdentityID() == the ENROLL-RETURNED id (the tracker's SPKI-hash identity)
// while Sign() still uses the real Ed25519 identity key. The tracker resolves
// PeerPubkey(env.Body.ConsumerId) against its SPKI-keyed mTLS table, so a
// raw-pubkey-hash ConsumerId (= identity.Signer.IdentityID()) would come back
// ErrUnauthenticated; the signature itself is verified against the mTLS raw
// pubkey, so signing is unaffected by the id swap.
type enrollSigner struct {
	signer   *identity.Signer
	enrollID ids.IdentityID
}

func (s enrollSigner) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	return signing.SignEnvelope(s.signer.PrivateKey(), body)
}

func (s enrollSigner) IdentityID() ids.IdentityID { return s.enrollID }

// consumerConfig is the POST /config body — MUST match the e2e driver's
// ConsumerConfig json tags (tracker/test/e2e/driver/actors.go) verbatim.
// Nil pointers default to the actor's normal behavior (true).
type consumerConfig struct {
	Settle *bool `json:"settle"`
	Dial   *bool `json:"dial"`
}

// requestSpec is the POST /request body — driver RequestSpec shape.
type requestSpec struct {
	Model           string `json:"model"`
	MaxInputTokens  uint32 `json:"max_input_tokens"`
	MaxOutputTokens uint32 `json:"max_output_tokens"`
}

// requestResult is the POST /request response — driver RequestResult shape.
// Outcome mirrors BrokerRequestResponse's oneof field names
// ("seeder_assignment", "no_capacity", "queued", "rejected"), plus "error"
// for flow failures.
type requestResult struct {
	Outcome             string `json:"outcome"`
	SeederAddr          string `json:"seeder_addr,omitempty"`
	SeederPubkeyHex     string `json:"seeder_pubkey_hex,omitempty"`
	ReservationTokenHex string `json:"reservation_token_hex,omitempty"`
	ResponseBody        string `json:"response_body,omitempty"`
	Error               string `json:"error,omitempty"`
}

// settlementInfo is the GET /settlement/last response — driver
// SettlementInfo shape.
type settlementInfo struct {
	RequestIDHex string `json:"request_id_hex"`
	Signed       bool   `json:"signed"`
	SettledAt    string `json:"settled_at,omitempty"`
}

// transferSpec is the POST /transfer body — driver TransferSpec shape.
type transferSpec struct {
	Amount     uint64 `json:"amount"`
	DestRegion string `json:"dest_region"`
}

// transferResult is the POST /transfer response — driver TransferResult shape.
type transferResult struct {
	SourceChainTipHashHex string `json:"source_chain_tip_hash_hex"`
	SourceSeq             uint64 `json:"source_seq"`
	Error                 string `json:"error,omitempty"`
}

// actorBalance is the GET /balance response — driver ActorBalance shape.
type actorBalance struct {
	Credits     int64  `json:"credits"`
	ChainTipSeq uint64 `json:"chain_tip_seq"`
	IssuedAt    uint64 `json:"issued_at"`
	ExpiresAt   uint64 `json:"expires_at"`
}

// consumer is the consumer-role state machine layered onto the Actor: the
// /request Balance→proof→envelope→BrokerRequest→tunnel flow, the
// trackerclient.SettlementHandler that counter-signs pushed settlements with
// the IDENTITY key, and the cross-region /transfer against tracker B.
//
// It is constructed BEFORE the trackerclient (Config.SettlementHandler is
// fixed at New time); the back-reference to the Actor is wired right after.
type consumer struct {
	log zerolog.Logger

	// seederTunnelPort, when non-zero, replaces the PORT of the broker's
	// SeederAddr before the tunnel dial: the tracker forwards the seeder's
	// QUIC-connection remote addr, whose port is the seeder's trackerclient
	// socket — NOT its tunnel listener. The compose topology pins the
	// seeder's --tunnel-addr to this fixed port.
	seederTunnelPort uint16

	// ctx spans the consumer's background work (settlement counter-sign
	// RPCs, the tracker-B client); cancelled by stop() on actor shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	// actor is set by newActor right after the Actor is built, before any
	// goroutine that reads it can run (pushes arrive only post-Start).
	actor *Actor

	mu             sync.Mutex
	settle         bool // counter-sign pushed settlements (default true)
	dial           bool // dial the assigned seeder tunnel (default true)
	lastSettlement *settlementInfo
	nonceCounter   uint64

	trackerBOnce sync.Once
	trackerB     *trackerclient.Client
	trackerBErr  error
}

func newConsumer(seederTunnelPort uint16, log zerolog.Logger) *consumer {
	ctx, cancel := context.WithCancel(context.Background())
	return &consumer{
		log:              log.With().Str("role", "consumer").Logger(),
		seederTunnelPort: seederTunnelPort,
		ctx:              ctx,
		cancel:           cancel,
		settle:           true,
		dial:             true,
	}
}

// stop cancels background work and closes the tracker-B client if one was
// ever opened.
func (c *consumer) stop() {
	c.cancel()
	c.mu.Lock()
	cli := c.trackerB
	c.mu.Unlock()
	if cli != nil {
		_ = cli.Close()
	}
}

// setConfig applies the driver ConsumerConfig toggles. A nil field resets
// the toggle to its default (true) — each POST /config is a full statement.
func (c *consumer) setConfig(cfg consumerConfig) {
	settle, dial := true, true
	if cfg.Settle != nil {
		settle = *cfg.Settle
	}
	if cfg.Dial != nil {
		dial = *cfg.Dial
	}
	c.mu.Lock()
	c.settle = settle
	c.dial = dial
	c.mu.Unlock()
}

func (c *consumer) toggles() (settle, dial bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settle, c.dial
}

// runRequest executes the §5 consumer flow with fabricated inputs: Balance
// (fresh, tracker-signed — the actor cannot forge it), a well-formed
// ExhaustionProofV1, a signed EnvelopeSigned whose ConsumerId is the ENROLL
// id and whose ConsumerEphemeralPub is this request's fresh ephemeral key,
// then BrokerRequest and — on assignment, when dialing is enabled — the
// tunnel dial + canned /v1/messages send + SSE read.
func (c *consumer) runRequest(ctx context.Context, spec requestSpec) requestResult {
	fail := func(stage string, err error) requestResult {
		c.log.Warn().Err(err).Str("stage", stage).Msg("request flow failed")
		return requestResult{Outcome: "error", Error: stage + ": " + err.Error()}
	}

	// Fresh per-request ephemeral keypair: its PUB rides in the envelope
	// (EnvelopeBody.ConsumerEphemeralPub → the seeder pins it), its PRIV
	// authenticates the tunnel dial. They MUST be the same key.
	ephPub, ephPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fail("ephemeral keygen", err)
	}

	balCtx, balCancel := context.WithTimeout(ctx, balanceTimeout)
	defer balCancel()
	balance, err := c.actor.client.Balance(balCtx, c.actor.enrollIdentityID())
	if err != nil {
		return fail("balance", err)
	}

	now := time.Now()
	proof, err := exhaustionproofbuilder.NewBuilder().Build(exhaustionproofbuilder.ProofInput{
		StopFailureMatcher:    "rate_limit",
		StopFailureAt:         now,
		StopFailureErrorShape: []byte(`{"type":"rate_limit_error","message":"tokenbay-e2e fabricated"}`),
		UsageProbeAt:          now,
		UsageProbeOutput:      []byte("tokenbay-e2e fabricated /usage output"),
	})
	if err != nil {
		return fail("proof build", err)
	}

	body := cannedMessagesBody(spec.Model, spec.MaxOutputTokens)
	bodyHash := sha256.Sum256(body)

	builder := envelopebuilder.NewBuilder(enrollSigner{
		signer:   c.actor.signer,
		enrollID: c.actor.enrollIdentityID(),
	})
	env, err := builder.Build(envelopebuilder.RequestSpec{
		Model:                spec.Model,
		MaxInputTokens:       uint64(spec.MaxInputTokens),
		MaxOutputTokens:      uint64(spec.MaxOutputTokens),
		Tier:                 tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:             bodyHash[:],
		ConsumerEphemeralPub: ephPub,
	}, proof, balance)
	if err != nil {
		return fail("envelope build", err)
	}

	brokerCtx, brokerCancel := context.WithTimeout(ctx, brokerTimeout)
	defer brokerCancel()
	res, err := c.actor.client.BrokerRequest(brokerCtx, env)
	if err != nil {
		return fail("broker request", err)
	}

	switch res.Outcome {
	case trackerclient.BrokerOutcomeAssignment:
		out := requestResult{
			Outcome:             "seeder_assignment",
			SeederAddr:          res.Assignment.SeederAddr,
			SeederPubkeyHex:     hex.EncodeToString(res.Assignment.SeederPubkey),
			ReservationTokenHex: hex.EncodeToString(res.Assignment.ReservationToken),
		}
		if _, dial := c.toggles(); !dial {
			c.log.Info().Str("seeder_addr", out.SeederAddr).Msg("assignment received; dial disabled, abandoning")
			return out
		}
		sse, err := c.dialAndServe(ctx, res.Assignment, ephPriv, body)
		if err != nil {
			out.Error = "tunnel: " + err.Error()
			c.log.Warn().Err(err).Str("seeder_addr", out.SeederAddr).Msg("tunnel flow failed")
			return out
		}
		out.ResponseBody = string(sse)
		return out
	case trackerclient.BrokerOutcomeNoCapacity:
		return requestResult{Outcome: "no_capacity", Error: res.NoCap.Reason}
	case trackerclient.BrokerOutcomeQueued:
		return requestResult{Outcome: "queued"}
	case trackerclient.BrokerOutcomeRejected:
		return requestResult{Outcome: "rejected", Error: fmt.Sprintf("reason=%d retry_after_s=%d", res.Rejected.Reason, res.Rejected.RetryAfterS)}
	default:
		return fail("broker outcome", fmt.Errorf("unknown outcome %d", res.Outcome))
	}
}

// dialAndServe dials the seeder's tunnel pinned by (this request's ephemeral
// priv, the assignment's seeder ephemeral pub), sends the canned
// /v1/messages body, and reads the SSE stream to EOF.
func (c *consumer) dialAndServe(ctx context.Context, asg *trackerclient.SeederAssignment, ephPriv ed25519.PrivateKey, body []byte) ([]byte, error) {
	addrPort, err := c.tunnelAddr(asg.SeederAddr)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, tunnelBudget)
	defer cancel()
	tun, err := tunnel.Dial(dialCtx, addrPort, tunnel.Config{
		EphemeralPriv: ephPriv,
		PeerPin:       ed25519.PublicKey(asg.SeederPubkey),
	})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addrPort, err)
	}
	defer tun.Close()

	if err := tun.Send(body); err != nil {
		return nil, fmt.Errorf("send request body: %w", err)
	}
	status, rdr, err := tun.Receive(dialCtx)
	if err != nil {
		return nil, fmt.Errorf("receive: %w", err)
	}
	sse, err := io.ReadAll(rdr)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if status != tunnel.StatusOK {
		return nil, fmt.Errorf("seeder returned status %v: %s", status, sse)
	}
	c.log.Info().Int("sse_bytes", len(sse)).Str("seeder_addr", asg.SeederAddr).Msg("tunnel request served")
	return sse, nil
}

// tunnelAddr derives the seeder's TUNNEL address from the broker's
// SeederAddr: keep the host, substitute the fixed --seeder-tunnel-port when
// configured (the SeederAddr port is the seeder's trackerclient socket, not
// its tunnel listener).
func (c *consumer) tunnelAddr(seederAddr string) (netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(seederAddr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse seeder addr %q: %w", seederAddr, err)
	}
	if c.seederTunnelPort != 0 {
		portStr = strconv.Itoa(int(c.seederTunnelPort))
	}
	// The tracker forwards the observed QUIC remote addr, so host is
	// normally an IP already; ResolveUDPAddr also tolerates a hostname.
	ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, portStr))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve tunnel addr %s:%s: %w", host, portStr, err)
	}
	ip, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("bad tunnel IP for %s:%s", host, portStr)
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(ua.Port)), nil //nolint:gosec // UDP port range
}

// cannedMessagesBody is the fixed /v1/messages request the consumer sends
// over the tunnel. Its SHA-256 is the envelope BodyHash. No Anthropic key,
// no claude binary — the seeder actor serves a canned SSE regardless.
func cannedMessagesBody(model string, maxOutputTokens uint32) []byte {
	return fmt.Appendf(nil,
		`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":"token-bay e2e canned request"}]}`,
		model, maxOutputTokens)
}

// HandleSettlement implements trackerclient.SettlementHandler. It verifies
// sha256(preimage_body) == preimage_hash, then — unless the settle toggle is
// off (dispute scenario) — counter-signs the RAW preimage bytes with the
// IDENTITY key (the tracker verifies via signing.VerifyUsageAssertion under
// the consumer pubkey from the mTLS connection) and delivers the sig through
// the unary Settle RPC. A non-nil return suppresses the push-stream SettleAck
// so the tracker records a dispute.
func (c *consumer) HandleSettlement(_ trackerclient.Ctx, r *trackerclient.SettlementRequest) error {
	if r == nil {
		return errors.New("consumer: nil settlement request")
	}
	if sha256.Sum256(r.PreimageBody) != r.PreimageHash {
		c.recordSettlement(settlementInfo{Signed: false})
		return errors.New("consumer: preimage hash mismatch")
	}
	rid, err := parseAssertionRequestID(r.PreimageBody)
	if err != nil {
		c.recordSettlement(settlementInfo{Signed: false})
		return fmt.Errorf("consumer: parse usage-assertion preimage: %w", err)
	}
	ridHex := hex.EncodeToString(rid[:])

	if settle, _ := c.toggles(); !settle {
		c.recordSettlement(settlementInfo{RequestIDHex: ridHex, Signed: false})
		c.log.Info().Str("request_id", ridHex).Msg("settlement push refused (settle=false)")
		return errors.New("consumer: settle disabled by /config")
	}

	sig, err := c.actor.signer.Sign(r.PreimageBody)
	if err != nil {
		c.recordSettlement(settlementInfo{RequestIDHex: ridHex, Signed: false})
		return fmt.Errorf("consumer: counter-sign: %w", err)
	}
	sctx, cancel := context.WithTimeout(c.ctx, settleTimeout)
	defer cancel()
	if err := c.actor.client.Settle(sctx, r.PreimageHash[:], sig); err != nil {
		c.recordSettlement(settlementInfo{RequestIDHex: ridHex, Signed: false})
		return fmt.Errorf("consumer: settle RPC: %w", err)
	}
	c.recordSettlement(settlementInfo{
		RequestIDHex: ridHex,
		Signed:       true,
		SettledAt:    time.Now().UTC().Format(time.RFC3339),
	})
	c.log.Info().Str("request_id", ridHex).Msg("settlement counter-signed")
	return nil
}

func (c *consumer) recordSettlement(info settlementInfo) {
	c.mu.Lock()
	c.lastSettlement = &info
	c.mu.Unlock()
}

// lastSettlementSnapshot returns the last SettlementHandler outcome
// (zero value if none yet).
func (c *consumer) lastSettlementSnapshot() settlementInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastSettlement == nil {
		return settlementInfo{}
	}
	return *c.lastSettlement
}

// parseAssertionRequestID extracts the 16-byte request_id from a canonical
// usage-assertion preimage (shared/signing layout:
// domain \0 request_id(16) \0 consumer_id(32) \0 ...). Only the request_id
// is needed here (for /settlement/last); the counter-signature is over the
// raw bytes, never a re-encoding.
func parseAssertionRequestID(body []byte) ([16]byte, error) {
	var rid [16]byte
	prefix := []byte(usageAssertionDomain)
	if len(body) < len(prefix)+18 || !bytes.HasPrefix(body, prefix) {
		return rid, errors.New("domain tag mismatch")
	}
	rest := body[len(prefix):]
	if rest[0] != 0 || rest[17] != 0 {
		return rid, errors.New("malformed field separators")
	}
	copy(rid[:], rest[1:17])
	return rid, nil
}

// runTransfer drives a cross-region TransferRequest against tracker B (the
// destination). SourceTrackerID/DestTrackerID are the FEDERATION tracker_ids
// (sha256 of raw Ed25519 pubkeys — from --source-fedid-file/--dest-fedid-file),
// NOT the SPKI-hash mTLS pins; the trackerclient signs the federation-
// canonical TransferProofRequest with the identity key internally.
func (c *consumer) runTransfer(ctx context.Context, spec transferSpec) transferResult {
	opts := c.actor.opts
	if opts.SourceFedID == ([32]byte{}) || opts.DestFedID == ([32]byte{}) {
		return transferResult{Error: "transfer: --source-fedid-file/--dest-fedid-file not configured"}
	}
	cli, err := c.trackerBClient(ctx)
	if err != nil {
		return transferResult{Error: "transfer: tracker B: " + err.Error()}
	}

	tctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	enrollID := c.actor.enrollIdentityID()
	proof, err := cli.TransferRequest(tctx, &trackerclient.TransferRequest{
		IdentityID:      enrollID,
		Amount:          spec.Amount,
		DestRegion:      spec.DestRegion,
		Nonce:           c.nextTransferNonce(enrollID),
		SourceTrackerID: opts.SourceFedID,
		DestTrackerID:   opts.DestFedID,
		Timestamp:       uint64(time.Now().Unix()), //nolint:gosec // post-1970 clock
	})
	if err != nil {
		return transferResult{Error: "transfer: " + err.Error()}
	}
	return transferResult{
		SourceChainTipHashHex: hex.EncodeToString(proof.SourceChainTipHash[:]),
		SourceSeq:             proof.SourceSeq,
	}
}

// nextTransferNonce derives a deterministic-per-actor, unique-per-call
// 32-byte nonce: sha256(enroll_id || counter). Random enough for the ledger
// TransferRef, reproducible enough for tests.
func (c *consumer) nextTransferNonce(enrollID ids.IdentityID) [32]byte {
	c.mu.Lock()
	c.nonceCounter++
	n := c.nonceCounter
	c.mu.Unlock()
	var buf [40]byte
	copy(buf[:32], enrollID[:])
	binary.BigEndian.PutUint64(buf[32:], n)
	return sha256.Sum256(buf[:])
}

// trackerBClient lazily builds, starts, and connects the second
// trackerclient to the transfer destination. Built lazily (not at actor
// boot) so a down tracker B never blocks the primary enroll path.
func (c *consumer) trackerBClient(ctx context.Context) (*trackerclient.Client, error) {
	c.trackerBOnce.Do(func() {
		opts := c.actor.opts
		if opts.TrackerBAddr == "" {
			c.trackerBErr = errors.New("--tracker-b-addr not configured")
			return
		}
		cfg := trackerclient.Config{
			Endpoints: []trackerclient.TrackerEndpoint{{
				Addr:         opts.TrackerBAddr,
				IdentityHash: opts.TrackerBHash,
			}},
			Identity: c.actor.signer,
			Logger:   c.log.With().Str("tracker", "b").Logger(),
		}
		switch {
		case opts.TransportB != nil:
			cfg.Transport = opts.TransportB
		case opts.Transport != nil:
			cfg.Transport = opts.Transport
		}
		cli, err := trackerclient.New(cfg)
		if err != nil {
			c.trackerBErr = fmt.Errorf("build client: %w", err)
			return
		}
		// The client's supervisor lifetime is the consumer's, not one
		// request's — stop() closes it.
		if err := cli.Start(c.ctx); err != nil {
			c.trackerBErr = fmt.Errorf("start client: %w", err)
			return
		}
		c.mu.Lock()
		c.trackerB = cli
		c.mu.Unlock()
	})
	if c.trackerBErr != nil {
		return nil, c.trackerBErr
	}
	c.mu.Lock()
	cli := c.trackerB
	c.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := cli.WaitConnected(waitCtx); err != nil {
		return nil, fmt.Errorf("connect %s: %w", c.actor.opts.TrackerBAddr, err)
	}
	return cli, nil
}

// fetchBalance serves GET /balance from a fresh Balance RPC under the
// enroll id (driver ActorBalance shape).
func (c *consumer) fetchBalance(ctx context.Context) (actorBalance, error) {
	bctx, cancel := context.WithTimeout(ctx, balanceTimeout)
	defer cancel()
	snap, err := c.actor.client.Balance(bctx, c.actor.enrollIdentityID())
	if err != nil {
		return actorBalance{}, err
	}
	return actorBalance{
		Credits:     snap.Body.Credits,
		ChainTipSeq: snap.Body.ChainTipSeq,
		IssuedAt:    snap.Body.IssuedAt,
		ExpiresAt:   snap.Body.ExpiresAt,
	}, nil
}
