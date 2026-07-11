package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/harness"
	"github.com/token-bay/token-bay/plugin/internal/tunnel"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// seederFixture bundles the running seeder actor, the fakeserver tracker it
// is wired to, and channels observing the tracker-side RPCs the seeder makes.
type seederFixture struct {
	base     string // control API base URL
	fake     *fakeserver.Server
	seederID []byte // tracker-issued enroll id (32 bytes)
	adverts  chan *tbproto.Advertisement
	usages   chan *tbproto.UsageReport
}

// startSeederActor boots a --role seeder actor against an in-process
// fakeserver and blocks until /healthz reports ready (connected + enrolled).
// The tunnel listener binds an ephemeral port (--tunnel-addr 127.0.0.1:0).
func startSeederActor(t *testing.T) *seederFixture {
	t.Helper()
	return startSeederActorWithTunnelAddr(t, "127.0.0.1:0")
}

// startSeederActorWithTunnelAddr is startSeederActor with an explicit
// --tunnel-addr, so a test can pin the seeder's tunnel bind to a fixed
// port (the Docker topology's configuration) instead of the default
// ephemeral one.
func startSeederActorWithTunnelAddr(t *testing.T, tunnelAddr string) *seederFixture {
	t.Helper()
	const addr = "tracker-a:0"

	seederID := make([]byte, 32)
	for i := range seederID {
		seederID[i] = byte(0xC0 + i%16)
	}

	srv, transport := harness.Loopback(addr)
	adverts := make(chan *tbproto.Advertisement, 16)
	usages := make(chan *tbproto.UsageReport, 16)

	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		er, ok := req.(*tbproto.EnrollRequest)
		require.True(t, ok, "enroll handler got %T", req)
		assert.Equal(t, RoleSeeder, er.Role)
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.EnrollResponse{
			IdentityId:          seederID,
			StarterGrantCredits: 100,
		}, nil
	}
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_ADVERTISE] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		ad, ok := req.(*tbproto.Advertisement)
		require.True(t, ok, "advertise handler got %T", req)
		select {
		case adverts <- ad:
		default:
		}
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.AdvertiseAck{}, nil
	}
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		ur, ok := req.(*tbproto.UsageReport)
		require.True(t, ok, "usage handler got %T", req)
		select {
		case usages <- ur:
		default:
		}
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.UsageAck{}, nil
	}

	serverDone := make(chan struct{})
	go func() {
		_ = srv.Run(context.Background())
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = srv.Conn.Close()
		<-serverDone
	})

	var trackerHash [32]byte
	trackerHash[0] = 0x11

	actor, err := newActor(options{
		Role:        RoleSeeder,
		RoleName:    "seeder",
		TrackerAddr: addr,
		TrackerHash: trackerHash,
		Region:      "A",
		DataDir:     t.TempDir(),
		CtrlAddr:    "127.0.0.1:0",
		TunnelAddr:  tunnelAddr,
		Transport:   transport,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- actor.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("actor.run did not return after cancel")
		}
	})

	base := "http://" + actor.CtrlAddr()
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // test poll
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "/healthz never returned 200")

	return &seederFixture{base: base, fake: srv, seederID: seederID, adverts: adverts, usages: usages}
}

func ctlGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test request
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

// TestSeederActor_OfferServeUsageReport drives the full seeder happy path:
// /config → Advertise, offer push → accept with a fresh 32-byte ephemeral
// pub + pinned tunnel listener, tunnel dial → canned SSE served, then a
// UsageReport signed with the EPHEMERAL key over the usage-assertion.
func TestSeederActor_OfferServeUsageReport(t *testing.T) {
	fx := startSeederActor(t)

	const model = "claude-sonnet-4-6"
	const sseBody = "event: message_start\ndata: {\"canned\":true}\n\n"

	// POST /config — driver SeederConfig shape (json tags verbatim).
	cfgBody := []byte(`{"available":true,"headroom":0.5,"models":["` + model + `"],"max_context":200000,"tiers":1,"sse_body":"event: message_start\ndata: {\"canned\":true}\n\n"}`)
	resp, err := http.Post(fx.base+"/config", "application/json", bytes.NewReader(cfgBody)) //nolint:noctx // test request
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "POST /config must succeed")

	// The advertise loop must push the configured advertisement.
	select {
	case ad := <-fx.adverts:
		assert.True(t, ad.Available)
		assert.InDelta(t, 0.5, float64(ad.Headroom), 1e-6)
		assert.Equal(t, []string{model}, ad.Models)
		assert.Equal(t, uint32(200000), ad.MaxContext)
		assert.Equal(t, uint32(1), ad.Tiers)
	case <-time.After(5 * time.Second):
		t.Fatal("no Advertise RPC after POST /config")
	}

	// Consumer per-session ephemeral keypair — its pub rides in the offer.
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	consumerID := make([]byte, 32)
	for i := range consumerID {
		consumerID[i] = byte(0xA0 + i%16)
	}
	reqID := make([]byte, 16)
	for i := range reqID {
		reqID[i] = byte(0x10 + i)
	}

	dec, err := fx.fake.PushOffer(context.Background(), &tbproto.OfferPush{
		ConsumerId:           consumerID,
		EnvelopeHash:         make([]byte, 32),
		Model:                model,
		MaxInputTokens:       4096,
		MaxOutputTokens:      1024,
		ConsumerEphemeralPub: consumerPub,
		RequestId:            reqID,
	})
	require.NoError(t, err)
	require.True(t, dec.Accept, "seeder must accept the offer (reason=%q)", dec.RejectReason)
	require.Len(t, dec.EphemeralPubkey, 32, "decision must carry a 32-byte seeder ephemeral pub")
	seederEphPub := ed25519.PublicKey(dec.EphemeralPubkey)

	// /offers/last reflects the accepted offer (driver OfferInfo shape) and
	// exposes the tunnel listen address (extra field, ignored by the driver).
	var offerInfo struct {
		ConsumerIDHex      string `json:"consumer_id_hex"`
		RequestIDHex       string `json:"request_id_hex"`
		Model              string `json:"model"`
		Accepted           bool   `json:"accepted"`
		EphemeralPubkeyHex string `json:"ephemeral_pubkey_hex"`
		RejectReason       string `json:"reject_reason"`
		TunnelAddr         string `json:"tunnel_addr"`
	}
	ctlGetJSON(t, fx.base+"/offers/last", &offerInfo)
	assert.Equal(t, hex.EncodeToString(consumerID), offerInfo.ConsumerIDHex)
	assert.Equal(t, hex.EncodeToString(reqID), offerInfo.RequestIDHex)
	assert.Equal(t, model, offerInfo.Model)
	assert.True(t, offerInfo.Accepted)
	assert.Equal(t, hex.EncodeToString(dec.EphemeralPubkey), offerInfo.EphemeralPubkeyHex)
	assert.Empty(t, offerInfo.RejectReason)
	require.NotEmpty(t, offerInfo.TunnelAddr, "accepted offer must expose the tunnel listen addr")

	// Dial the pinned tunnel as the consumer and read the canned SSE.
	tunnelAddr, err := netip.ParseAddrPort(offerInfo.TunnelAddr)
	require.NoError(t, err)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	tun, err := tunnel.Dial(dialCtx, tunnelAddr, tunnel.Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederEphPub,
	})
	require.NoError(t, err)
	defer tun.Close()

	require.NoError(t, tun.Send([]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	status, rdr, err := tun.Receive(dialCtx)
	require.NoError(t, err)
	require.Equal(t, tunnel.StatusOK, status)
	got, err := io.ReadAll(rdr)
	require.NoError(t, err)
	assert.Equal(t, sseBody, string(got), "tunnel must serve the configured canned SSE")

	// The seeder must follow up with a UsageReport signed by the EPHEMERAL key.
	var report *tbproto.UsageReport
	select {
	case report = <-fx.usages:
	case <-time.After(5 * time.Second):
		t.Fatal("no UsageReport RPC after serving the tunnel")
	}
	assert.Equal(t, reqID, report.RequestId)
	assert.Equal(t, model, report.Model)
	// The reported usage is pinned to the offer's own MaxInputTokens/
	// MaxOutputTokens (4096/1024 above), not a fixed canned value — see
	// servedOffer's doc comment: reporting exactly what was reserved keeps
	// actual cost == reserved cost so the tracker's overspend guard
	// (broker/settlement.go, 5% tolerance) never rejects it regardless of
	// what a test requests.
	assert.Equal(t, uint32(4096), report.InputTokens)
	assert.Equal(t, uint32(1024), report.OutputTokens)

	// cost = 3*4096 + 15*1024 = 27648 (mirrored sonnet pricing 3 in / 15 out).
	const wantCost = uint64(27648)
	assertion := signing.UsageAssertion{
		RequestID:    reqID,
		ConsumerID:   consumerID,
		SeederID:     fx.seederID,
		Model:        model,
		InputTokens:  report.InputTokens,
		OutputTokens: report.OutputTokens,
		CostCredits:  wantCost,
	}
	assert.True(t, signing.VerifyUsageAssertion(seederEphPub, assertion, report.SeederSig),
		"UsageReport sig must verify under the seeder EPHEMERAL pub over the usage-assertion")

	// /usage/last reflects the report (driver UsageInfo shape).
	var usageInfo struct {
		RequestIDHex string `json:"request_id_hex"`
		InputTokens  uint32 `json:"input_tokens"`
		OutputTokens uint32 `json:"output_tokens"`
		Model        string `json:"model"`
		CostCredits  uint64 `json:"cost_credits"`
	}
	require.Eventually(t, func() bool {
		ctlGetJSON(t, fx.base+"/usage/last", &usageInfo)
		return usageInfo.RequestIDHex != ""
	}, 5*time.Second, 20*time.Millisecond, "/usage/last never populated")
	assert.Equal(t, hex.EncodeToString(reqID), usageInfo.RequestIDHex)
	assert.Equal(t, uint32(4096), usageInfo.InputTokens)
	assert.Equal(t, uint32(1024), usageInfo.OutputTokens)
	assert.Equal(t, model, usageInfo.Model)
	assert.Equal(t, wantCost, usageInfo.CostCredits)
}

// TestSeederActor_RejectsOfferWithoutEphemeralPub asserts the no_ephemeral
// reject path: an offer without the consumer's 32-byte ephemeral pub cannot
// pin the tunnel and must be refused.
func TestSeederActor_RejectsOfferWithoutEphemeralPub(t *testing.T) {
	fx := startSeederActor(t)

	consumerID := make([]byte, 32)
	reqID := make([]byte, 16)
	dec, err := fx.fake.PushOffer(context.Background(), &tbproto.OfferPush{
		ConsumerId:   consumerID,
		EnvelopeHash: make([]byte, 32),
		Model:        "claude-sonnet-4-6",
		RequestId:    reqID,
	})
	require.NoError(t, err)
	assert.False(t, dec.Accept)
	assert.Equal(t, "no_ephemeral", dec.RejectReason)
	assert.Empty(t, dec.EphemeralPubkey)

	var offerInfo struct {
		Accepted     bool   `json:"accepted"`
		RejectReason string `json:"reject_reason"`
	}
	ctlGetJSON(t, fx.base+"/offers/last", &offerInfo)
	assert.False(t, offerInfo.Accepted)
	assert.Equal(t, "no_ephemeral", offerInfo.RejectReason)
}

// TestSeederActor_ReusesFixedTunnelPortAfterAbandonedOffer guards against a
// fixed-port re-listen ordering regression: in the Docker topology the
// seeder's tunnel listener binds a FIXED port (--tunnel-addr), so if an
// offer is accepted but the consumer never dials, the old listener would
// hold that port for up to serveWindow (60s) unless HandleOffer closes it
// BEFORE binding the next offer's tunnel.Listen on the same port.
//
// This drives two offers through the SAME fixed tunnel port: the first is
// accepted and then abandoned (never dialed), the second must still be
// accepted with a working tunnel.Listen — no "address in use" reject.
func TestSeederActor_ReusesFixedTunnelPortAfterAbandonedOffer(t *testing.T) {
	// Discover a free loopback UDP port, then pin the actor's
	// --tunnel-addr to that exact port so both offers below bind the SAME
	// fixed port and exercise the close-before-bind ordering fix.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	fixedAddr := probe.LocalAddr().(*net.UDPAddr).String()
	require.NoError(t, probe.Close())

	fx := startSeederActorWithTunnelAddr(t, fixedAddr)

	const model = "claude-sonnet-4-6"
	cfgBody := []byte(`{"available":true,"headroom":0.5,"models":["` + model + `"],"max_context":200000,"tiers":1,"sse_body":"data: canned\n\n"}`)
	resp, err := http.Post(fx.base+"/config", "application/json", bytes.NewReader(cfgBody)) //nolint:noctx // test request
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "POST /config must succeed")

	select {
	case <-fx.adverts:
	case <-time.After(5 * time.Second):
		t.Fatal("no Advertise RPC after POST /config")
	}

	pushOffer := func(idByte byte) *tbproto.OfferDecision {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		dec, err := fx.fake.PushOffer(context.Background(), &tbproto.OfferPush{
			ConsumerId:           bytes.Repeat([]byte{idByte}, 32),
			EnvelopeHash:         make([]byte, 32),
			Model:                model,
			MaxInputTokens:       4096,
			MaxOutputTokens:      1024,
			ConsumerEphemeralPub: pub,
			RequestId:            bytes.Repeat([]byte{idByte}, 16),
		})
		require.NoError(t, err)
		return dec
	}

	// First offer: accepted, tunnel bound on the fixed port — then
	// abandoned (the consumer never dials it), which is exactly the
	// scenario that used to strand the fixed port for serveWindow.
	dec1 := pushOffer(0xA1)
	require.True(t, dec1.Accept, "first offer must be accepted (reason=%q)", dec1.RejectReason)

	var info1 offerInfo
	ctlGetJSON(t, fx.base+"/offers/last", &info1)
	require.Equal(t, fixedAddr, info1.TunnelAddr, "first tunnel must bind the fixed --tunnel-addr port")

	// Second offer arrives on the SAME fixed port well within the first
	// offer's serveWindow. Without closing the abandoned listener before
	// rebinding, tunnel.Listen here fails with "tunnel_listen: ... address
	// in use" and the offer is rejected.
	dec2 := pushOffer(0xB2)
	require.True(t, dec2.Accept, "second offer must be accepted despite the fixed tunnel port (reason=%q)", dec2.RejectReason)
	require.Len(t, dec2.EphemeralPubkey, 32, "decision must carry a 32-byte seeder ephemeral pub")

	var info2 offerInfo
	ctlGetJSON(t, fx.base+"/offers/last", &info2)
	assert.Empty(t, info2.RejectReason)
	assert.True(t, info2.Accepted)
	assert.Equal(t, fixedAddr, info2.TunnelAddr, "second tunnel must reuse the same fixed port")
}
