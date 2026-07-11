//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// ---------------------------------------------------------------------------
// TestIntegration_BrokerAssign proves the headline path unlocked by Task 2
// (seeders now register in the registry on connect): a seeder that connects
// and ADVERTISEs becomes selectable by the broker, and a consumer's real
// BROKER_REQUEST RPC — sent over its own mTLS QUIC connection, dispatched
// through the same api.Router/server.Server stack production uses — comes
// back a SeederAssignment instead of NoCapacity.
//
// Unlike broker_e2e_test.go (which calls subs.Broker.Submit in-process,
// skipping the api layer entirely) and helpers_test.go's newFixture (whose
// router has no Broker/Admission wired, so BROKER_REQUEST is a stub), this
// test brings up a fixture with both: real ledger + registry + admission +
// broker + api.Router + server.Server, reachable only via the wire.
// ---------------------------------------------------------------------------

// brokerAssignFixture is newBrokerAssignFixture's bringup, analogous to
// helpers_test.go's fixture but with Broker/Admission/Settlement wired into
// the router so BROKER_REQUEST and ADVERTISE are both live over the wire.
type brokerAssignFixture struct {
	srv    *server.Server
	led    *ledger.Ledger
	reg    *registry.Registry
	addr   string
	pin    [32]byte
	runErr chan error
	cancel context.CancelFunc
}

// brokerAssignAdmission satisfies api.AdmissionService (enrollAdmission ∪
// brokerAdmission) by embedding the real *admission.Subsystem — which
// already implements Decide + QueueTimeout — and adding a no-op Admit so
// the type also satisfies enrollAdmission. Mirrors the admissionAdapter
// pattern in cmd/token-bay-tracker/run_cmd.go. This test never calls
// ENROLL; Admit exists only so the value type-checks against Deps.Admission.
type brokerAssignAdmission struct {
	*admission.Subsystem
}

func (brokerAssignAdmission) Admit(context.Context, []byte, []byte) error { return nil }

// newBrokerAssignFixture brings up a real server + ledger + registry +
// admission + broker + api.Router, all wired the way cmd/token-bay-tracker
// wires them (minus federation/reputation, irrelevant here). pusher stands
// in for the real push-offer handshake to a connected seeder (see
// stubPusher in broker_e2e_test.go) — wiring the actual server-side push
// path is a separate concern from what Task 2/3 are proving.
func newBrokerAssignFixture(t *testing.T, pusher broker.PushService) *brokerAssignFixture {
	t.Helper()
	tmp := t.TempDir()

	_, srvPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyPath := filepath.Join(tmp, "tracker.key")
	if err := os.WriteFile(srvKeyPath, srvPriv, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := storage.Open(context.Background(), filepath.Join(tmp, "ledger.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	led, err := ledger.Open(store, srvPriv)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New(8)
	if err != nil {
		t.Fatal(err)
	}

	adm, err := admission.Open(
		defaultAdmissionConfig(),
		reg,
		srvPriv,
		admission.WithSnapshotPrefix(filepath.Join(tmp, "snapshot")),
		admission.WithTLogPath(filepath.Join(tmp, "tlog.bin")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })

	scfg := defaultSettlementE2EConfig()
	bcfg := defaultBrokerE2EConfig()
	subs, err := broker.Open(bcfg, scfg, broker.Deps{
		Logger:    zerolog.Nop(),
		Now:       time.Now,
		Registry:  reg,
		Ledger:    led,
		Admission: adm,
		Pusher:    pusher,
		Pricing:   broker.DefaultPriceTable(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subs.Close() })

	alloc, err := stunturn.NewAllocator(stunturn.AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              time.Now,
		Rand:             crand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}

	router, err := api.NewRouter(api.Deps{
		Logger:     zerolog.Nop(),
		Now:        time.Now,
		Ledger:     led,
		Registry:   reg,
		StunTurn:   stAdapter{alloc: alloc},
		Broker:     subs.Broker,
		Settlement: subs.Settlement,
		Admission:  brokerAssignAdmission{adm},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			ListenAddr:         "127.0.0.1:0",
			IdentityKeyPath:    srvKeyPath,
			MaxFrameSize:       1 << 20,
			IdleTimeoutS:       60,
			MaxIncomingStreams: 1024,
			ShutdownGraceS:     5,
		},
		Broker:     bcfg,
		Settlement: scfg,
	}

	srv, err := server.New(server.Deps{
		Config:   cfg,
		Logger:   zerolog.Nop(),
		Now:      time.Now,
		Registry: reg,
		Ledger:   led,
		StunTurn: alloc,
		Reflect:  func(a netip.AddrPort) netip.AddrPort { return a },
		API:      router,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(runCtx) }()

	if !waitForListen(srv, 2*time.Second) {
		runCancel()
		t.Fatal("listener never bound")
	}

	srvCert, err := server.CertFromIdentity(srvPriv)
	if err != nil {
		t.Fatal(err)
	}
	srvParsed, err := x509.ParseCertificate(srvCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(srvParsed.RawSubjectPublicKeyInfo)

	f := &brokerAssignFixture{
		srv:    srv,
		led:    led,
		reg:    reg,
		addr:   srv.ListenAddr(),
		pin:    pin,
		runErr: runErr,
		cancel: runCancel,
	}
	t.Cleanup(f.shutdown)
	return f
}

// waitForRegistryRecord polls reg for id up to timeout. Registration
// (Task 2's fix) happens in the server's serveConn goroutine as soon as the
// QUIC handshake completes, but opening a client-side stream (OpenStreamSync)
// does not synchronize with the server having run that far — mirrors the
// waitForPeers polling idiom in internal/server/server_test.go for the same
// race. Callers should pass a generous timeout (seconds, not hundreds of
// milliseconds): the poll exits as soon as the record appears, so a large
// budget costs nothing on the happy path but keeps the test robust when the
// full suite runs on a heavily loaded machine.
func waitForRegistryRecord(reg *registry.Registry, id ids.IdentityID, timeout time.Duration) (registry.SeederRecord, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if rec, ok := reg.Get(id); ok {
			return rec, true
		}
		if time.Now().After(deadline) {
			return registry.SeederRecord{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *brokerAssignFixture) shutdown() {
	dl, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(dl)
	f.cancel()
	select {
	case <-f.runErr:
	case <-time.After(2 * time.Second):
	}
}

// dialAs opens a QUIC connection to addr presenting a certificate derived
// from priv, pinning the server's certificate via pin. Mirrors
// (*fixture).dial's TLS construction (helpers_test.go) but takes the client
// key as a parameter — this test dials as two distinct peers (seeder and
// consumer) against one fixture, whereas the shared fixture's dial is bound
// to a single fixed cliPriv.
func dialAs(t *testing.T, addr string, pin [32]byte, priv ed25519.PrivateKey) *quicgo.Conn {
	t.Helper()
	cliCert, err := server.CertFromIdentity(priv)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cliCert},
		InsecureSkipVerify: true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
			if got != pin {
				return errors.New("client: server SPKI mismatch")
			}
			return nil
		},
		NextProtos:             []string{"tokenbay/1"},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quicgo.DialAddr(ctx, addr, tlsCfg, &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// mtlsIdentityID derives the IdentityID the server computes for a peer
// identified by priv: sha256 of the parsed cert's RawSubjectPublicKeyInfo.
// Matches helpers_test.go's inline cliPeer derivation and
// server.serveConn's own SPKIToIdentityID(state.PeerCertificates[0]) call —
// this is the "SPKI-hash IdentityID the tracker assigns via mTLS", distinct
// from a bare sha256(raw pubkey) (see broker_e2e_test.go's
// seederIdentityFromPub, which is not used here for exactly that reason).
func mtlsIdentityID(t *testing.T, priv ed25519.PrivateKey) ids.IdentityID {
	t.Helper()
	cert, err := server.CertFromIdentity(priv)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	id, err := server.SPKIToIdentityID(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// buildValidBrokerEnvelope builds an EnvelopeSigned that passes
// tbproto.ValidateEnvelopeBody (exercised by the real installBrokerRequest
// handler, unlike broker_e2e_test.go's buildEnvelopeWithBalance which skips
// the api layer entirely) with a real BalanceProof read fresh from led.
func buildValidBrokerEnvelope(t *testing.T, led *ledger.Ledger, consumerID ids.IdentityID, model string, maxIn, maxOut uint64) *tbproto.EnvelopeSigned {
	t.Helper()
	snap, err := led.SignedBalance(context.Background(), consumerID[:])
	if err != nil {
		t.Fatal(err)
	}
	ts := uint64(time.Now().Unix()) //nolint:gosec // G115 — unix seconds, always positive
	return &tbproto.EnvelopeSigned{
		Body: &tbproto.EnvelopeBody{
			ProtocolVersion: uint32(tbproto.ProtocolVersion),
			ConsumerId:      consumerID[:],
			Model:           model,
			MaxInputTokens:  maxIn,
			MaxOutputTokens: maxOut,
			Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
			BodyHash:        make([]byte, 32),
			ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
				StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
				UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
				CapturedAt:  ts,
				Nonce:       make([]byte, 16),
			},
			BalanceProof: snap,
			CapturedAt:   ts,
			Nonce:        make([]byte, 16),
		},
	}
}

func TestIntegration_BrokerAssign(t *testing.T) {
	const model = "claude-sonnet-4-6"

	// Seeder identity, generated before the fixture: the stub pusher echoes
	// this raw pubkey back as the offer's ephemeral key, standing in for the
	// real server-push handshake to a connected seeder (see stubPusher in
	// broker_e2e_test.go — every broker E2E test in this package uses the
	// same stand-in for exactly this reason).
	seederPub, seederPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pusher := &stubPusher{offerAccept: true, offerEphPub: seederPub}

	f := newBrokerAssignFixture(t, pusher)

	// --- Seeder: connect, then ADVERTISE over the wire. ---
	seederID := mtlsIdentityID(t, seederPriv)
	seederConn := dialAs(t, f.addr, f.pin, seederPriv)
	defer func() { _ = seederConn.CloseWithError(0, "bye") }()
	openHB(t, seederConn)

	// Task 2's fix registers every connecting peer as Available=false at
	// connect time; confirm that landed before ADVERTISE flips it. 5s budget:
	// normally this resolves in one or two 10ms polls, but the serveConn
	// goroutine can be scheduled late when the full integration suite runs
	// under load.
	preRec, ok := waitForRegistryRecord(f.reg, seederID, 5*time.Second)
	if !ok {
		t.Fatal("seeder record never appeared in the registry after connect")
	}
	if preRec.Available {
		t.Error("seeder record Available=true before advertise, want false")
	}

	adBody, err := proto.Marshal(&tbproto.Advertisement{
		Models:     []string{model},
		MaxContext: 200000,
		Available:  true,
		Headroom:   0.9, // >= broker's HeadroomThreshold (0.2)
		Tiers:      0x1, // bit0 = STANDARD
	})
	if err != nil {
		t.Fatal(err)
	}
	adResp := rpcSimple(t, seederConn, tbproto.RpcMethod_RPC_METHOD_ADVERTISE, adBody)
	if adResp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("advertise: status=%v error=%+v", adResp.Status, adResp.Error)
	}

	// The seeder must now be selectable: Available=true, and still carrying
	// the reflexive addr Task 2's connect-time fix populated (Advertise is a
	// partial update, not an upsert — see registry.Registry.Advertise).
	rec, ok := f.reg.Get(seederID)
	if !ok {
		t.Fatal("seeder record missing after advertise")
	}
	if !rec.Available {
		t.Error("seeder record Available=false after advertise, want true")
	}
	if !rec.NetCoords.ExternalAddr.IsValid() {
		t.Error("seeder record lost its connect-time reflexive addr after advertise")
	}
	gotSeederPub, ok := f.srv.PeerPubkey(seederID)
	if !ok || !bytes.Equal(gotSeederPub, seederPub) {
		t.Fatalf("server.PeerPubkey(seederID) = %x, %v; want %x, true", gotSeederPub, ok, seederPub)
	}

	// --- Consumer: connect, fund via the ledger, then BROKER_REQUEST. ---
	_, consumerPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	consumerID := mtlsIdentityID(t, consumerPriv)
	consumerConn := dialAs(t, f.addr, f.pin, consumerPriv)
	defer func() { _ = consumerConn.CloseWithError(0, "bye") }()
	openHB(t, consumerConn)

	// Fund the consumer directly against the fixture's ledger — the same
	// mechanism ENROLL's starter grant uses under the hood (enroll.go calls
	// led.IssueStarterGrant(ctx, rc.PeerID[:], ...)); calling it here keeps
	// the envelope-build idiom identical to broker_e2e_test.go's
	// buildEnvelopeWithBalance while keying credits off the real mTLS
	// IdentityID this connection was assigned.
	if _, err := f.led.IssueStarterGrant(context.Background(), consumerID[:], 1_000_000); err != nil {
		t.Fatal(err)
	}

	env := buildValidBrokerEnvelope(t, f.led, consumerID, model, 100, 200)
	envBytes, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	brResp := rpcSimple(t, consumerConn, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, envBytes)
	if brResp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("broker_request: status=%v error=%+v", brResp.Status, brResp.Error)
	}

	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(brResp.Payload, &brr); err != nil {
		t.Fatal(err)
	}

	switch outcome := brr.Outcome.(type) {
	case *tbproto.BrokerRequestResponse_SeederAssignment:
		sa := outcome.SeederAssignment
		if len(sa.SeederAddr) == 0 {
			t.Error("seeder_addr is empty, want the seeder's connect-time reflexive addr")
		}
		if !bytes.Equal(sa.SeederPubkey, seederPub) {
			t.Errorf("seeder_pubkey = %x, want %x (the advertised seeder's pubkey)", sa.SeederPubkey, seederPub)
		}
	case *tbproto.BrokerRequestResponse_NoCapacity:
		t.Fatalf("broker_request returned NoCapacity (reason=%q): the connect-time registration "+
			"fix did not make the advertised seeder selectable", outcome.NoCapacity.Reason)
	default:
		t.Fatalf("unexpected broker_request outcome %T: %+v", brr.Outcome, &brr)
	}
}
