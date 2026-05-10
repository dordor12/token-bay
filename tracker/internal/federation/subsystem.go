package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// Federation is the subsystem's public type.
type Federation struct {
	cfg Config
	dep Deps

	reg          *Registry
	dedupe       *Dedupe
	gossip       *Gossip
	apply        *RootAttestApplier
	equiv        *Equivocator
	pub          *Publisher
	transfer     *transferCoordinator
	revocation   *revocationCoordinator
	peerExchange *peerExchangeCoordinator
	health       *PeerHealth

	// listenCtx is the cancellable context Open creates. It scopes both
	// the Listen goroutine and the per-peer dial goroutines, AND is
	// threaded into acceptInbound so a Close during an in-flight
	// listener handshake aborts cleanly. Lock order: f.mu > reg.mu.
	listenCtx    context.Context
	listenCancel context.CancelFunc
	mu           sync.Mutex
	peers        map[ids.TrackerID]*Peer // active recv goroutines
	closed       bool
}

// Open constructs and starts the federation subsystem. It returns a ready
// *Federation or an error if required Deps are missing.
func Open(cfg Config, dep Deps) (*Federation, error) {
	cfg = cfg.withDefaults()
	if dep.Transport == nil {
		return nil, errors.New("federation: Transport required")
	}
	if dep.RootSrc == nil {
		return nil, errors.New("federation: RootSource required")
	}
	if dep.Archive == nil {
		return nil, errors.New("federation: Archive required")
	}
	if dep.Metrics == nil {
		return nil, errors.New("federation: Metrics required")
	}
	if dep.Now == nil {
		dep.Now = NowFromTime
	}

	reg := NewRegistry()
	dedupe := NewDedupe(cfg.DedupeTTL, cfg.DedupeCap, dep.Now)

	// Build gossip backed by registry + per-peer Send. peerSet is a
	// pointer so we can patch the *Federation back-reference after
	// constructing federation below.
	peerSet := &registryPeerSet{} // f filled in after Federation is built
	gossip := NewGossip(cfg.MyPriv, cfg.MyTrackerID, peerSet)

	// Wrap Forward into a closure that dedupes locally and counts metrics.
	forward := func(ctx context.Context, kind fed.Kind, payload []byte) {
		dedupe.Mark(sha256.Sum256(payload))
		_ = gossip.Forward(ctx, kind, payload, nil)
		dep.Metrics.FramesOut(kind.String())
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			dep.Metrics.RootAttestationsPublished()
		}
	}

	health := NewPeerHealth(cfg.Health, dep.Now, dep.Metrics.HealthScoreComputed)

	apply := NewRootAttestApplier(dep.Archive, forward, dep.Now)
	equiv := NewEquivocator(dep.Archive, forward, reg, health.OnEquivocation).WithSelf(cfg.MyTrackerID)
	apply.RegisterEquivocator(equiv.OnLocalConflict)

	pub := NewPublisher(dep.RootSrc, forward, cfg.MyTrackerID)

	transfer := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: cfg.MyTrackerID,
		MyPriv:      cfg.MyPriv,
		Ledger:      dep.Ledger,
		IssuedCap:   cfg.IssuedProofCap,
		Now:         dep.Now,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			info, ok := reg.Get(id)
			if !ok {
				return nil, false
			}
			return info.PubKey, true
		},
		// Send is patched after we have *Federation; transfer never
		// invokes it before f is constructed and the patch lands below.
		Send:           nil,
		MetricsCounter: func(name string) { dep.Metrics.InvalidFrames(name) },
	})

	revocation := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: cfg.MyTrackerID,
		MyPriv:      cfg.MyPriv,
		Archive:     dep.RevocationArchive,
		Forward:     forward,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			info, ok := reg.Get(id)
			if !ok {
				return nil, false
			}
			return info.PubKey, true
		},
		Now:                  dep.Now,
		Invalid:              func(name string) { dep.Metrics.InvalidFrames(name) },
		OnEmit:               dep.Metrics.RevocationsEmitted,
		OnReceived:           dep.Metrics.RevocationsReceived,
		OnRevocationObserved: health.OnRevocation,
	})

	var peerExchange *peerExchangeCoordinator
	if dep.KnownPeers != nil {
		peerExchange = newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
			MyTrackerID: cfg.MyTrackerID,
			MyPriv:      cfg.MyPriv,
			Archive:     dep.KnownPeers,
			Forward:     forward,
			Health:      health,
			Now:         dep.Now,
			EmitCap:     defaultPeerExchangeEmitCap,
			Invalid:     func(name string) { dep.Metrics.InvalidFrames(name) },
			OnEmit:      dep.Metrics.PeerExchangeEmitted,
			OnReceived:  dep.Metrics.PeerExchangeReceived,
		})
	}

	f := &Federation{
		cfg: cfg, dep: dep,
		reg: reg, dedupe: dedupe, gossip: gossip,
		apply: apply, equiv: equiv, pub: pub,
		transfer: transfer, revocation: revocation, peerExchange: peerExchange,
		health: health,
		peers:  make(map[ids.TrackerID]*Peer),
	}
	peerSet.f = f
	transfer.cfg.Send = func(ctx context.Context, peer ids.TrackerID, kind fed.Kind, payload []byte) error {
		return f.SendToPeer(ctx, peer, kind, payload)
	}
	// PeerConnected closure removed in slice 5 — peer-exchange now uses
	// *PeerHealth.Score (wired via cfg.Health below) instead of the
	// allowlist-connected placeholder.

	// Add operator-allowlisted peers in pending state. Dialing them is
	// kicked off below; this slice initializes their entries so they
	// are visible via Peers().
	for _, p := range cfg.Peers {
		_ = reg.Add(PeerInfo{TrackerID: p.TrackerID, PubKey: p.PubKey, Addr: p.Addr, Region: p.Region, State: PeerStatePending})
	}

	// Seed allowlisted peers into the known_peers archive (slice 3).
	// Idempotent: the SQL clause pins addr+region_hint for source='allowlist'
	// rows, so an operator config refresh takes effect (allowlist→allowlist)
	// while gossip echoes can never overwrite (gossip→allowlist blocked).
	if dep.KnownPeers != nil {
		seedCtx := context.Background()
		seenAt := dep.Now()
		for _, p := range cfg.Peers {
			tid := p.TrackerID.Bytes()
			_ = dep.KnownPeers.UpsertKnownPeer(seedCtx, storage.KnownPeer{
				TrackerID:   tid[:],
				Addr:        p.Addr,
				LastSeen:    seenAt,
				RegionHint:  p.Region,
				HealthScore: 0.5,
				Source:      "allowlist",
			})
		}
	}

	// Listen on the transport. The accept callback runs the listener-side
	// handshake and, on success, attaches a Peer recv loop.
	ctx, cancel := context.WithCancel(context.Background())
	f.listenCtx = ctx
	f.listenCancel = cancel
	go func() { _ = dep.Transport.Listen(ctx, f.acceptInbound) }()

	// Dial each operator-allowlisted peer in a Dialer goroutine. The
	// Dialer redials with exponential backoff after every drop; its
	// OnConnected callback runs the federation handshake and blocks on
	// the recvLoop, returning when the peer drops so the dialer
	// reconnects.
	for _, p := range cfg.Peers {
		d := &Dialer{
			Transport:   dep.Transport,
			Peer:        p,
			MyTrackerID: cfg.MyTrackerID,
			MyPriv:      cfg.MyPriv,
			HandshakeTO: cfg.HandshakeTimeout,
			BackoffBase: cfg.RedialBase,
			BackoffMax:  cfg.RedialMax,
			Now:         dep.Now,
			OnConnected: f.attachAndWait(p),
			OnFailure: func(reason string, _ error) {
				dep.Metrics.InvalidFrames(reason)
			},
		}
		go d.Run(ctx)
	}
	return f, nil
}

// attachAndWait returns an OnConnected callback for the Dialer. The
// callback runs the dialer-side federation handshake on the supplied
// PeerConn and, on success, attaches the peer to the registry and waits
// for its recvLoop to exit. Returning unblocks the Dialer's redial loop.
func (f *Federation) attachAndWait(p AllowlistedPeer) func(PeerConn) {
	return func(c PeerConn) {
		res, err := RunHandshakeDialer(f.listenCtx, c, f.cfg.MyTrackerID, f.cfg.MyPriv, p.TrackerID, p.PubKey, f.cfg.HandshakeTimeout)
		if err != nil {
			f.dep.Logger.Warn().Err(err).Str("peer", p.Addr).Msg("federation: dialer handshake failed")
			f.dep.Metrics.InvalidFrames("handshake")
			_ = c.Close()
			return
		}
		pe := f.attachPeerLocked(res, c)
		if pe == nil {
			// attachPeerLocked already closed c (subsystem was closed
			// concurrently); nothing to wait on.
			return
		}
		pe.Wait()
	}
}

// Peers is the operator-facing snapshot of all known peers.
func (f *Federation) Peers() []PeerInfo { return f.reg.All() }

// Depeer removes a peer from the active set.
func (f *Federation) Depeer(id ids.TrackerID, reason DepeerReason) error {
	f.mu.Lock()
	if p, ok := f.peers[id]; ok {
		p.Stop()
		delete(f.peers, id)
	}
	f.mu.Unlock()
	return f.reg.Depeer(id, reason)
}

// PublishHour drives a single publisher tick.
func (f *Federation) PublishHour(ctx context.Context, hour uint64) error {
	return f.pub.PublishHour(ctx, hour)
}

// PublishPeerExchange snapshots the local known_peers archive and
// broadcasts a KIND_PEER_EXCHANGE envelope to all active peers. v1
// ships the on-demand entry point only; production cadence is operator-
// driven (cron, admin endpoint, or future ticker driver in
// tracker/cmd/). Returns ErrPeerExchangeDisabled if the subsystem was
// opened without a KnownPeersArchive Dep.
func (f *Federation) PublishPeerExchange(ctx context.Context) error {
	if f == nil || f.peerExchange == nil {
		return ErrPeerExchangeDisabled
	}
	return f.peerExchange.EmitNow(ctx)
}

// StartTransfer is the api-handler entry point for a destination-side
// cross-region credit transfer. Returns ErrTransferDisabled if the
// federation was opened without a LedgerHooks dep, ErrPeerNotConnected
// if the source tracker isn't in steady state, ErrTransferTimeout on
// timeout, or the proof on success.
func (f *Federation) StartTransfer(ctx context.Context, in StartTransferInput) (StartTransferOutput, error) {
	if f.transfer == nil || f.transfer.cfg.Ledger == nil {
		return StartTransferOutput{}, ErrTransferDisabled
	}
	if !f.reg.IsActive(in.SourceTrackerID) {
		return StartTransferOutput{}, fmt.Errorf("%w: source=%x", ErrPeerNotConnected, in.SourceTrackerID.Bytes())
	}
	if f.cfg.TransferTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.cfg.TransferTimeout)
		defer cancel()
	}
	out, err := f.transfer.StartTransfer(ctx, in)
	if errors.Is(err, context.DeadlineExceeded) {
		return out, ErrTransferTimeout
	}
	return out, err
}

// OnFreeze implements reputation.FreezeListener via Go structural
// typing. The reputation evaluator calls this synchronously on every
// AUDIT->FROZEN transition; the call returns once the signed Revocation
// is on per-peer send queues. With no RevocationArchive Dep, the call
// is a no-op (revocation gossip is disabled at subsystem-open time).
func (f *Federation) OnFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	if f == nil || f.revocation == nil {
		return
	}
	f.revocation.OnFreeze(ctx, id, reason, revokedAt)
}

// ListenAddr returns the bound network address of the underlying
// transport, or "" if the transport is not network-bound (e.g. the
// in-process transport). Used by tests dialing 127.0.0.1:0 binds and
// by operator admin endpoints reporting the active peering port.
func (f *Federation) ListenAddr() string {
	if l, ok := f.dep.Transport.(interface{ ListenAddr() string }); ok {
		return l.ListenAddr()
	}
	return ""
}

// AddPeer adds a new operator-allowlisted peer at runtime, registering
// it in PeerStatePending and spawning a Dialer goroutine to bring it
// online. Returns ErrPeerExists if a peer with the same TrackerID is
// already known.
func (f *Federation) AddPeer(p AllowlistedPeer) error {
	if err := f.reg.Add(PeerInfo{
		TrackerID: p.TrackerID, PubKey: p.PubKey, Addr: p.Addr, Region: p.Region,
		State: PeerStatePending,
	}); err != nil {
		return err
	}
	d := &Dialer{
		Transport:   f.dep.Transport,
		Peer:        p,
		MyTrackerID: f.cfg.MyTrackerID,
		MyPriv:      f.cfg.MyPriv,
		HandshakeTO: f.cfg.HandshakeTimeout,
		BackoffBase: f.cfg.RedialBase,
		BackoffMax:  f.cfg.RedialMax,
		Now:         f.dep.Now,
		OnConnected: f.attachAndWait(p),
		OnFailure: func(reason string, _ error) {
			f.dep.Metrics.InvalidFrames(reason)
		},
	}
	go d.Run(f.listenCtx)
	return nil
}

// Close shuts down the subsystem, stopping all peer goroutines and closing
// the transport. Safe to call more than once.
func (f *Federation) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	for _, p := range f.peers {
		p.Stop()
	}
	f.peers = nil
	f.mu.Unlock()
	if f.listenCancel != nil {
		f.listenCancel()
	}
	return f.dep.Transport.Close()
}

func (f *Federation) acceptInbound(c PeerConn) {
	expected := map[ids.TrackerID]ed25519.PublicKey{}
	for _, p := range f.reg.All() {
		expected[p.TrackerID] = p.PubKey
	}
	// Use the Open-scoped ctx so a Close mid-handshake aborts cleanly
	// instead of waiting for the transport's per-conn timeout.
	res, err := RunHandshakeListener(f.listenCtx, c, f.cfg.MyTrackerID, f.cfg.MyPriv, expected, f.cfg.HandshakeTimeout)
	if err != nil {
		f.dep.Logger.Warn().Err(err).Str("addr", c.RemoteAddr()).Msg("federation: listener handshake failed")
		f.dep.Metrics.InvalidFrames("handshake")
		_ = c.Close()
		return
	}
	f.attachPeer(res, c)
}

func (f *Federation) attachPeer(res HandshakeResult, c PeerConn) {
	f.attachPeerLocked(res, c)
}

// attachPeerLocked is the shared body of attachPeer + the Dialer's
// OnConnected hook. Returns the *Peer so callers can Wait() on its
// recvLoop, or nil if the subsystem is already closed. Lock order:
// f.mu > reg.mu (see Registry).
//
// The early closed-check is intentional: building the dispatcher and
// Peer first would create a fresh sync.Once whose memory could be
// recycled and concurrently accessed by Transport.Close's pair-Close
// path during test teardown — the race detector flags this even though
// the accesses are logically disjoint.
func (f *Federation) attachPeerLocked(res HandshakeResult, c PeerConn) *Peer {
	f.mu.Lock()
	if f.closed {
		// Close() already nilled f.peers; a dialer arriving after Close
		// drops the connection here. Don't start the recv loop; the
		// transport's own Close path tears the conn down.
		f.mu.Unlock()
		return nil
	}
	dispatch := f.makeDispatcher(c, res.PeerTrackerID)
	pe := NewPeerForTest(c, dispatch)
	f.peers[res.PeerTrackerID] = pe
	_ = f.reg.Update(PeerInfo{TrackerID: res.PeerTrackerID, PubKey: res.PeerPubKey, Addr: c.RemoteAddr(), State: PeerStateSteady, Conn: c})
	f.mu.Unlock()
	pe.Start(context.Background())
	return pe
}

// makeDispatcher returns the recvLoop callback for one peer. The
// callback verifies the envelope sig, dedupes by message_id, dispatches
// by kind to apply or equiv, and counts metrics.
func (f *Federation) makeDispatcher(c PeerConn, peerID ids.TrackerID) func(*fed.Envelope) {
	return func(env *fed.Envelope) {
		f.dep.Metrics.FramesIn(env.Kind.String())
		if err := VerifyEnvelope(c.RemotePub(), env); err != nil {
			f.dep.Metrics.InvalidFrames("sig")
			return
		}
		mid := MessageID(env)
		if f.dedupe.Seen(mid) {
			f.dep.Metrics.InvalidFrames("dedupe_replay")
			return
		}
		f.dedupe.Mark(mid)
		switch env.Kind {
		case fed.Kind_KIND_ROOT_ATTESTATION:
			if err := f.apply.Apply(context.Background(), env); err != nil {
				if errors.Is(err, ErrEquivocation) {
					f.dep.Metrics.EquivocationsDetected()
				}
				f.dep.Metrics.RootAttestationsReceived(equivOutcome(err))
				return
			}
			f.dep.Metrics.RootAttestationsReceived("archived")
			f.health.OnRootAttestation(peerID, f.dep.Now())
		case fed.Kind_KIND_EQUIVOCATION_EVIDENCE:
			f.equiv.OnIncomingEvidence(context.Background(), env, peerID)
		case fed.Kind_KIND_PING, fed.Kind_KIND_PONG:
			// keepalive — no action in this slice beyond the metric.
		case fed.Kind_KIND_TRANSFER_PROOF_REQUEST:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnRequest(context.Background(), env, peerID)
		case fed.Kind_KIND_TRANSFER_PROOF:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnProof(context.Background(), env, peerID)
		case fed.Kind_KIND_TRANSFER_APPLIED:
			if f.transfer == nil {
				f.dep.Metrics.InvalidFrames("transfer_disabled")
				return
			}
			f.transfer.OnApplied(context.Background(), env, peerID)
		case fed.Kind_KIND_REVOCATION:
			if f.revocation == nil {
				f.dep.Metrics.InvalidFrames("revocation_disabled")
				return
			}
			f.revocation.OnIncoming(context.Background(), env, peerID)
		case fed.Kind_KIND_PEER_EXCHANGE:
			if f.peerExchange == nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_disabled")
				f.dep.Metrics.PeerExchangeReceived("peer_exchange_disabled")
				return
			}
			var msg fed.PeerExchange
			if err := proto.Unmarshal(env.Payload, &msg); err != nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_unmarshal")
				return
			}
			if err := fed.ValidatePeerExchange(&msg); err != nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_shape")
				return
			}
			f.peerExchange.OnIncoming(context.Background(), env, &msg)
		default:
			f.dep.Metrics.InvalidFrames(fmt.Sprintf("kind_%d", int(env.Kind)))
		}
	}
}

func equivOutcome(err error) string {
	switch {
	case err == nil:
		return "archived"
	case errors.Is(err, ErrEquivocation):
		return "conflict"
	default:
		return "error"
	}
}

// registryPeerSet adapts *Registry into PeerSet by walking its active
// peers and returning the live *Peer behind each. Pointer receiver so
// the subsystem can fill f after Federation is constructed.
type registryPeerSet struct{ f *Federation }

func (r *registryPeerSet) ActivePeers() []SendOnlyPeer {
	if r == nil || r.f == nil {
		return nil
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	out := make([]SendOnlyPeer, 0, len(r.f.peers))
	for id, pe := range r.f.peers {
		out = append(out, peerSendAdapter{id: id, peer: pe})
	}
	return out
}

type peerSendAdapter struct {
	id   ids.TrackerID
	peer *Peer
}

func (p peerSendAdapter) ID() ids.TrackerID { return p.id }
func (p peerSendAdapter) Send(ctx context.Context, frame []byte) error {
	return p.peer.Send(ctx, frame)
}

// ApplyForTest is a test-only shim: build a ROOT_ATTESTATION envelope
// from (peerID, peerPriv, hour, root, sig) and route it through the
// applier. Returns the applier's error verbatim. Bypasses the recv loop
// on purpose — full recv coverage lives in the real-transport slice.
//
// The peerPriv parameter signs the envelope; the test caller must keep
// it consistent with the peerID (sha256(pubkey) == peerID) for the
// applier's tracker_id-vs-sender_id check to pass.
func (f *Federation) ApplyForTest(peerID ids.TrackerID, peerPriv ed25519.PrivateKey, hour uint64, root, sig []byte) error {
	idBytes := peerID.Bytes()
	msg := &fed.RootAttestation{TrackerId: idBytes[:], Hour: hour, MerkleRoot: root, TrackerSig: sig}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	env, err := SignEnvelope(peerPriv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err != nil {
		return err
	}
	return f.apply.Apply(context.Background(), env)
}
