package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/plugin/internal/tunnel"
	"github.com/token-bay/token-bay/shared/signing"
)

// defaultTunnelBind is the tunnel listener bind when --tunnel-addr is not
// given: every interface, ephemeral port. The Docker topology overrides it
// with a fixed port so the consumer can derive the tunnel address from the
// broker's SeederAddr host (the tracker forwards the seeder's QUIC-connection
// remote addr, whose PORT is the trackerclient socket — not the tunnel).
const defaultTunnelBind = "0.0.0.0:0"

// advertisePeriod is the re-advertise cadence once /config has been set.
// Heartbeat freshness is handled by trackerclient (15s automatic); this loop
// only re-asserts the advertisement across tracker restarts/reconnects.
const advertisePeriod = 5 * time.Second

// advertiseTimeout bounds one Advertise RPC.
const advertiseTimeout = 3 * time.Second

// serveWindow bounds how long an accepted offer's tunnel listener waits for
// the consumer to dial + complete the serve before giving up. Comfortably
// above the tracker's tunnel_setup_ms (500ms) and settlement_timeout_s (3s).
const serveWindow = 60 * time.Second

// usageReportTimeout bounds the post-serve UsageReport RPC.
const usageReportTimeout = 5 * time.Second

// Fallback token counts used only when an offer omits MaxInputTokens/
// MaxOutputTokens (the zero-value case OfferPush's doc comment calls out for
// a legacy tracker build) — every offer from this e2e stack's tracker
// populates them, so the normal path is fallbackInputTokens/
// fallbackOutputTokens (below).
const (
	cannedInputTokens  uint32 = 128
	cannedOutputTokens uint32 = 256
)

// e2ePriceEntry mirrors tracker/internal/broker.ModelPrices.
type e2ePriceEntry struct {
	inCreditsPerToken  uint64
	outCreditsPerToken uint64
}

// e2ePriceTable mirrors tracker/internal/broker.DefaultPriceTable — the same
// values plugin/internal/seederflow/pricing.go mirrors (its cost helper is
// unexported, so the actor carries its own copy). cost_credits is part of
// the canonical usage-assertion; the tracker recomputes it with its own
// table and rejects the seeder sig if the numbers differ.
var e2ePriceTable = map[string]e2ePriceEntry{
	"claude-opus-4-7":           {inCreditsPerToken: 15, outCreditsPerToken: 75},
	"claude-sonnet-4-6":         {inCreditsPerToken: 3, outCreditsPerToken: 15},
	"claude-haiku-4-5-20251001": {inCreditsPerToken: 1, outCreditsPerToken: 5},
}

// costCredits replicates broker.PriceTable.ActualCost:
// cost = in_price*input_tokens + out_price*output_tokens.
func costCredits(model string, in, out uint32) (uint64, error) {
	p, ok := e2ePriceTable[model]
	if !ok {
		return 0, fmt.Errorf("no mirrored price for model %q", model)
	}
	return p.inCreditsPerToken*uint64(in) + p.outCreditsPerToken*uint64(out), nil
}

// seederConfig is the POST /config body — MUST match the e2e driver's
// SeederConfig json tags (tracker/test/e2e/driver/actors.go) verbatim.
type seederConfig struct {
	Available  bool     `json:"available"`
	Headroom   float64  `json:"headroom"`
	Models     []string `json:"models"`
	MaxContext uint32   `json:"max_context"`
	Tiers      uint32   `json:"tiers"`
	SSEBody    string   `json:"sse_body"`
}

// offerInfo is the GET /offers/last response — driver OfferInfo shape.
// tunnel_addr is an extra debugging/test field the driver ignores.
type offerInfo struct {
	ConsumerIDHex      string `json:"consumer_id_hex"`
	RequestIDHex       string `json:"request_id_hex"`
	Model              string `json:"model"`
	Accepted           bool   `json:"accepted"`
	EphemeralPubkeyHex string `json:"ephemeral_pubkey_hex,omitempty"`
	RejectReason       string `json:"reject_reason,omitempty"`
	TunnelAddr         string `json:"tunnel_addr,omitempty"`
}

// usageInfo is the GET /usage/last response — driver UsageInfo shape.
type usageInfo struct {
	RequestIDHex string `json:"request_id_hex"`
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	Model        string `json:"model"`
	CostCredits  uint64 `json:"cost_credits"`
}

// servedOffer is the per-offer state the serve goroutine needs, snapshotted
// at HandleOffer time so later /config changes don't affect in-flight serves.
//
// inputTokens/outputTokens are the counts the UsageReport claims — pinned to
// the offer's OWN MaxInputTokens/MaxOutputTokens (falling back to
// cannedInputTokens/cannedOutputTokens only when the offer omits them). The
// tracker's settlement path rejects a usage_report whose
// Pricing.ActualCost(...) exceeds the request's own reserved MaxCost by more
// than 5% (broker/settlement.go's overspend guard) — a FIXED canned cost
// regardless of what was requested works only for requests that happen to
// reserve at least that much, which silently breaks for any smaller/cheaper
// request a test chooses to make. Reporting exactly what the request
// reserved keeps actual == reserved for every request shape, so the e2e
// settlement scenarios can pick whatever MaxInputTokens/MaxOutputTokens suit
// the consumer's starter-grant budget without hand-tuning against a
// hardcoded seeder cost.
type servedOffer struct {
	consumerID   [32]byte
	requestID    [16]byte
	model        string
	sseBody      string
	inputTokens  uint32
	outputTokens uint32
}

// seeder is the seeder-role state machine layered onto the Actor: an
// advertise loop driven by POST /config, a trackerclient.OfferHandler that
// pins a per-offer tunnel listener, the canned-SSE tunnel server, and the
// ephemeral-key-signed UsageReport that follows a served request.
//
// It is constructed BEFORE the trackerclient (Config.OfferHandler is fixed
// at New time); the back-reference to the Actor is wired right after.
type seeder struct {
	log        zerolog.Logger
	tunnelBind netip.AddrPort

	// ctx spans the seeder's background work (advertise loop, per-offer
	// serve goroutines); cancelled by stop() when the actor shuts down.
	ctx    context.Context
	cancel context.CancelFunc

	// kick wakes the advertise loop immediately after POST /config.
	kick chan struct{}

	// actor is set by newActor right after the Actor is built, before any
	// goroutine that reads it can run (offers arrive only post-Start).
	actor *Actor

	mu        sync.Mutex
	cfg       seederConfig
	cfgSet    bool
	lastOffer *offerInfo
	lastUsage *usageInfo
	activeLn  *tunnel.Listener // last offer's listener; superseded offers close it
}

func newSeeder(bind netip.AddrPort, log zerolog.Logger) *seeder {
	ctx, cancel := context.WithCancel(context.Background())
	return &seeder{
		log:        log.With().Str("role", "seeder").Logger(),
		tunnelBind: bind,
		ctx:        ctx,
		cancel:     cancel,
		kick:       make(chan struct{}, 1),
	}
}

// stop cancels background work and closes any live tunnel listener.
func (s *seeder) stop() {
	s.cancel()
	s.closeActiveListener()
}

// setConfig stores the advertised config + canned SSE body and kicks the
// advertise loop.
func (s *seeder) setConfig(cfg seederConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.cfgSet = true
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// advertiseLoop pushes the configured advertisement immediately on each
// /config and re-asserts it every advertisePeriod. Runs until stop().
func (s *seeder) advertiseLoop() {
	ticker := time.NewTicker(advertisePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-ticker.C:
		}
		s.advertiseOnce()
	}
}

func (s *seeder) advertiseOnce() {
	s.mu.Lock()
	cfg, set := s.cfg, s.cfgSet
	s.mu.Unlock()
	if !set {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, advertiseTimeout)
	defer cancel()
	err := s.actor.client.Advertise(ctx, &trackerclient.Advertisement{
		Models:     cfg.Models,
		MaxContext: cfg.MaxContext,
		Available:  cfg.Available,
		Headroom:   float32(cfg.Headroom),
		Tiers:      cfg.Tiers,
	})
	if err != nil {
		s.log.Warn().Err(err).Msg("advertise failed")
		return
	}
	s.log.Debug().Bool("available", cfg.Available).Float64("headroom", cfg.Headroom).
		Strs("models", cfg.Models).Msg("advertised")
}

// HandleOffer implements trackerclient.OfferHandler. It must return within
// the tracker's offer_timeout_ms (1500ms default); everything here is local
// (keygen + UDP bind), so it does. The tunnel listener is pinned to
// (fresh seeder ephemeral priv, consumer ephemeral pub from the offer) and
// served on a background goroutine.
func (s *seeder) HandleOffer(_ trackerclient.Ctx, o *trackerclient.Offer) (trackerclient.OfferDecision, error) {
	info := offerInfo{
		ConsumerIDHex: hex.EncodeToString(o.ConsumerID[:]),
		RequestIDHex:  hex.EncodeToString(o.RequestID[:]),
		Model:         o.Model,
	}
	reject := func(reason string) (trackerclient.OfferDecision, error) {
		info.RejectReason = reason
		s.recordOffer(info)
		s.log.Info().Str("reason", reason).Str("model", o.Model).Msg("offer rejected")
		return trackerclient.OfferDecision{Accept: false, RejectReason: reason}, nil
	}

	if len(o.ConsumerEphemeralPub) != ed25519.PublicKeySize {
		return reject("no_ephemeral")
	}
	if _, err := costCredits(o.Model, 1, 1); err != nil {
		return reject("unpriced_model")
	}

	ephPub, ephPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return reject("keygen: " + err.Error())
	}

	// Copy the pin out of the push buffer before it leaves scope.
	peerPin := append(ed25519.PublicKey(nil), o.ConsumerEphemeralPub...)

	// Close any previously-active listener BEFORE binding the new one. The
	// Docker topology's --tunnel-addr is a FIXED port, so if the prior
	// offer's listener is still open — abandoned (consumer never dialed)
	// or superseded mid-serve — the tunnel.Listen below fails with
	// "address in use" until that listener's serveWindow (60s) elapses.
	// tunnel.Listener.Close is idempotent (tracks its own closed state),
	// so this is harmless even if serveOffer's own deferred Close on the
	// same listener already ran.
	s.closeActiveListener()

	ln, err := tunnel.Listen(s.tunnelBind, tunnel.Config{
		EphemeralPriv: ephPriv,
		PeerPin:       peerPin,
	})
	if err != nil {
		return reject("tunnel_listen: " + err.Error())
	}
	s.setActiveListener(ln)

	inTok, outTok := o.MaxInputTokens, o.MaxOutputTokens
	if inTok == 0 {
		inTok = cannedInputTokens
	}
	if outTok == 0 {
		outTok = cannedOutputTokens
	}
	off := servedOffer{
		consumerID:   o.ConsumerID,
		requestID:    o.RequestID,
		model:        o.Model,
		sseBody:      s.currentSSEBody(),
		inputTokens:  inTok,
		outputTokens: outTok,
	}
	go s.serveOffer(ln, off, ephPriv)

	info.Accepted = true
	info.EphemeralPubkeyHex = hex.EncodeToString(ephPub)
	info.TunnelAddr = ln.LocalAddr().String()
	s.recordOffer(info)
	s.log.Info().Str("model", o.Model).Str("tunnel_addr", info.TunnelAddr).Msg("offer accepted")
	return trackerclient.OfferDecision{Accept: true, EphemeralPubkey: ephPub}, nil
}

// serveOffer waits for the consumer to dial the pinned listener, serves the
// canned SSE over the half-duplex tunnel protocol (ReadRequest → SendOK →
// body → CloseWrite), then sends the ephemeral-key-signed UsageReport.
func (s *seeder) serveOffer(ln *tunnel.Listener, off servedOffer, ephPriv ed25519.PrivateKey) {
	defer ln.Close()
	ctx, cancel := context.WithTimeout(s.ctx, serveWindow)
	defer cancel()

	tun, err := ln.Accept(ctx)
	if err != nil {
		s.log.Warn().Err(err).Msg("tunnel accept failed (consumer never dialed?)")
		return
	}
	defer tun.Close()

	if _, err := tun.ReadRequest(); err != nil {
		s.log.Warn().Err(err).Msg("tunnel read request failed")
		_ = tun.SendError("e2e-actor: read request: " + err.Error())
		_ = tun.CloseWrite()
		return
	}
	if err := tun.SendOK(); err != nil {
		s.log.Warn().Err(err).Msg("tunnel send OK failed")
		return
	}
	if _, err := io.WriteString(tun.ResponseWriter(), off.sseBody); err != nil {
		s.log.Warn().Err(err).Msg("tunnel write SSE body failed")
		return
	}
	if err := tun.CloseWrite(); err != nil {
		s.log.Warn().Err(err).Msg("tunnel close write failed")
		return
	}
	s.reportUsage(off, ephPriv)
}

// reportUsage signs the usage-assertion with the PER-OFFER EPHEMERAL key —
// the tracker verifies the sig against the ephemeral pub the seeder returned
// in OfferDecision (NOT the identity key) — and sends the UsageReport RPC.
//
// Deliberately NOT derived from serveOffer's serveWindow ctx: that context
// can be within usageReportTimeout of its own deadline (a slow consumer
// dial near the 60s edge), which would silently truncate this RPC. A fresh
// background context with its own bound keeps the report window full-length
// regardless of where in the serve window it fires.
func (s *seeder) reportUsage(off servedOffer, ephPriv ed25519.PrivateKey) {
	cost, err := costCredits(off.model, off.inputTokens, off.outputTokens)
	if err != nil {
		s.log.Error().Err(err).Msg("cost lookup failed (should have been rejected at offer time)")
		return
	}
	seederID := s.actor.enrollIdentityID()
	sig, err := signing.SignUsageAssertion(ephPriv, signing.UsageAssertion{
		RequestID:    off.requestID[:],
		ConsumerID:   off.consumerID[:],
		SeederID:     seederID[:],
		Model:        off.model,
		InputTokens:  off.inputTokens,
		OutputTokens: off.outputTokens,
		CostCredits:  cost,
	})
	if err != nil {
		s.log.Error().Err(err).Msg("sign usage assertion failed")
		return
	}

	rctx, cancel := context.WithTimeout(context.Background(), usageReportTimeout)
	defer cancel()
	err = s.actor.client.UsageReport(rctx, &trackerclient.UsageReport{
		RequestID:    uuid.UUID(off.requestID),
		InputTokens:  off.inputTokens,
		OutputTokens: off.outputTokens,
		Model:        off.model,
		SeederSig:    sig,
	})
	if err != nil {
		s.log.Error().Err(err).Msg("usage report RPC failed")
		return
	}
	s.recordUsage(usageInfo{
		RequestIDHex: hex.EncodeToString(off.requestID[:]),
		InputTokens:  off.inputTokens,
		OutputTokens: off.outputTokens,
		Model:        off.model,
		CostCredits:  cost,
	})
	s.log.Info().Str("model", off.model).Uint64("cost_credits", cost).Msg("usage reported")
}

// closeActiveListener closes and clears the currently-active tunnel
// listener, if any. Called before HandleOffer binds a new one, and by
// stop(). tunnel.Listener.Close is idempotent, so this is safe to call
// even if the listener was already closed elsewhere (e.g. serveOffer's own
// deferred Close after a served or timed-out offer).
func (s *seeder) closeActiveListener() {
	s.mu.Lock()
	old := s.activeLn
	s.activeLn = nil
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// setActiveListener installs ln as the live listener.
func (s *seeder) setActiveListener(ln *tunnel.Listener) {
	s.mu.Lock()
	s.activeLn = ln
	s.mu.Unlock()
}

func (s *seeder) currentSSEBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.SSEBody
}

func (s *seeder) recordOffer(info offerInfo) {
	s.mu.Lock()
	s.lastOffer = &info
	s.mu.Unlock()
}

func (s *seeder) recordUsage(info usageInfo) {
	s.mu.Lock()
	s.lastUsage = &info
	s.mu.Unlock()
}

// lastOfferSnapshot returns the last handled offer (zero value if none yet).
func (s *seeder) lastOfferSnapshot() offerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastOffer == nil {
		return offerInfo{}
	}
	return *s.lastOffer
}

// lastUsageSnapshot returns the last sent usage report (zero value if none).
func (s *seeder) lastUsageSnapshot() usageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUsage == nil {
		return usageInfo{}
	}
	return *s.lastUsage
}
