package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/harness"
	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// consumerFixture bundles a running --role consumer actor, the fakeserver
// tracker A it enrolls with, an optional fakeserver tracker B (transfer
// target), and channels observing the tracker-side RPCs the consumer makes.
type consumerFixture struct {
	base     string // control API base URL
	fake     *fakeserver.Server
	fakeB    *fakeserver.Server // nil unless tracker B is wired
	enrollID []byte             // tracker-issued enroll id (32 bytes, SPKI-hash style)
	pubkey   ed25519.PublicKey  // the actor's identity pubkey (from /identity)

	balanceIDs chan []byte                  // identity ids the Balance RPC looked up
	envelopes  chan *tbproto.EnvelopeSigned // BrokerRequest envelopes
	settles    chan *tbproto.SettleRequest  // Settle RPCs
	transfersB chan *tbproto.TransferRequest
}

// fabricated federation tracker_ids (sha256-raw-pubkey style) for transfer
// tests — the unit test only asserts they flow into the signed request.
var (
	testSourceFedID = func() [32]byte { return sha256.Sum256([]byte("fedid-tracker-a")) }()
	testDestFedID   = func() [32]byte { return sha256.Sum256([]byte("fedid-tracker-b")) }()
)

// testAssignment is what tracker A's broker hands back on BrokerRequest.
func testAssignment() *tbproto.SeederAssignment {
	pk := make([]byte, 32)
	for i := range pk {
		pk[i] = byte(0xE0 + i%16)
	}
	token := make([]byte, 16)
	for i := range token {
		token[i] = byte(0x20 + i)
	}
	return &tbproto.SeederAssignment{
		SeederAddr:       []byte("203.0.113.7:9443"),
		SeederPubkey:     pk,
		ReservationToken: token,
	}
}

// startConsumerActor boots a --role consumer actor against an in-process
// fakeserver tracker A (and, when withTrackerB, a second fakeserver as the
// transfer destination) and blocks until /healthz reports ready.
func startConsumerActor(t *testing.T, withTrackerB bool) *consumerFixture {
	t.Helper()
	const addrA = "tracker-a:0"
	const addrB = "tracker-b:0"

	enrollID := make([]byte, 32)
	for i := range enrollID {
		enrollID[i] = byte(0xD0 + i%16)
	}

	fx := &consumerFixture{
		enrollID:   enrollID,
		balanceIDs: make(chan []byte, 16),
		envelopes:  make(chan *tbproto.EnvelopeSigned, 16),
		settles:    make(chan *tbproto.SettleRequest, 16),
		transfersB: make(chan *tbproto.TransferRequest, 16),
	}

	srv, transport := harness.Loopback(addrA)
	fx.fake = srv

	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		er, ok := req.(*tbproto.EnrollRequest)
		require.True(t, ok, "enroll handler got %T", req)
		assert.Equal(t, RoleConsumer, er.Role)
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.EnrollResponse{
			IdentityId:          enrollID,
			StarterGrantCredits: 1_000_000,
		}, nil
	}
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_BALANCE] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		br, ok := req.(*tbproto.BalanceRequest)
		require.True(t, ok, "balance handler got %T", req)
		select {
		case fx.balanceIDs <- br.IdentityId:
		default:
		}
		now := uint64(time.Now().Unix())
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.SignedBalanceSnapshot{
			Body: &tbproto.BalanceSnapshotBody{
				IdentityId:   br.IdentityId,
				Credits:      1_000_000,
				ChainTipHash: make([]byte, 32),
				ChainTipSeq:  42,
				IssuedAt:     now,
				ExpiresAt:    now + 600,
			},
			TrackerSig: make([]byte, 64),
		}, nil
	}
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		env, ok := req.(*tbproto.EnvelopeSigned)
		require.True(t, ok, "broker handler got %T", req)
		select {
		case fx.envelopes <- env:
		default:
		}
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerRequestResponse{
			Outcome: &tbproto.BrokerRequestResponse_SeederAssignment{SeederAssignment: testAssignment()},
		}, nil
	}
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_SETTLE] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		sr, ok := req.(*tbproto.SettleRequest)
		require.True(t, ok, "settle handler got %T", req)
		select {
		case fx.settles <- sr:
		default:
		}
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.SettleAck{}, nil
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

	opts := options{
		Role:        RoleConsumer,
		RoleName:    "consumer",
		TrackerAddr: addrA,
		TrackerHash: trackerHash,
		Region:      "A",
		DataDir:     t.TempDir(),
		CtrlAddr:    "127.0.0.1:0",
		Transport:   transport,
	}

	if withTrackerB {
		srvB, transportB := harness.Loopback(addrB)
		fx.fakeB = srvB
		srvB.Handlers[tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			tr, ok := req.(*tbproto.TransferRequest)
			require.True(t, ok, "transfer handler got %T", req)
			select {
			case fx.transfersB <- tr:
			default:
			}
			tip := make([]byte, 32)
			for i := range tip {
				tip[i] = byte(0x77)
			}
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TransferProof{
				SourceChainTipHash: tip,
				SourceSeq:          7,
				TrackerSig:         make([]byte, 64),
			}, nil
		}
		serverBDone := make(chan struct{})
		go func() {
			_ = srvB.Run(context.Background())
			close(serverBDone)
		}()
		t.Cleanup(func() {
			_ = srvB.Conn.Close()
			<-serverBDone
		})

		var trackerBHash [32]byte
		trackerBHash[0] = 0x22
		opts.TrackerBAddr = addrB
		opts.TrackerBHash = trackerBHash
		opts.TransportB = transportB
		opts.SourceFedID = testSourceFedID
		opts.DestFedID = testDestFedID
	}

	actor, err := newActor(opts)
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

	fx.base = "http://" + actor.CtrlAddr()
	require.Eventually(t, func() bool {
		resp, err := http.Get(fx.base + "/healthz") //nolint:noctx // test poll
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "/healthz never returned 200")

	var idResp struct {
		PubkeyHex string `json:"pubkey_hex"`
	}
	ctlGetJSON(t, fx.base+"/identity", &idResp)
	pub, err := hex.DecodeString(idResp.PubkeyHex)
	require.NoError(t, err)
	fx.pubkey = ed25519.PublicKey(pub)

	return fx
}

func ctlPostJSON(t *testing.T, url string, body []byte, out any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx // test request
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

// pushSettlementFor builds the canonical usage-assertion preimage for the
// reservation token of testAssignment and pushes it at the consumer.
func pushSettlementFor(t *testing.T, fx *consumerFixture, model string) (preimage []byte, hash [32]byte, requestID []byte, err error) {
	t.Helper()
	asg := testAssignment()
	requestID = asg.ReservationToken

	seederID := make([]byte, 32)
	for i := range seederID {
		seederID[i] = byte(0xC0 + i%16)
	}
	cost, cErr := costCredits(model, cannedInputTokens, cannedOutputTokens)
	require.NoError(t, cErr)

	preimage, cErr = signing.CanonicalUsageAssertionPreSig(signing.UsageAssertion{
		RequestID:    requestID,
		ConsumerID:   fx.enrollID,
		SeederID:     seederID,
		Model:        model,
		InputTokens:  cannedInputTokens,
		OutputTokens: cannedOutputTokens,
		CostCredits:  cost,
	})
	require.NoError(t, cErr)
	hash = sha256.Sum256(preimage)

	pushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = fx.fake.PushSettlement(pushCtx, &tbproto.SettlementPush{
		PreimageHash: hash[:],
		PreimageBody: preimage,
	})
	return preimage, hash, requestID, err
}

// TestConsumerActor_RequestBrokerSettlementFlow drives the consumer happy
// path against the fakeserver: POST /request builds a valid envelope whose
// ConsumerId is the ENROLL-returned id (not sha256(rawPubkey)) with a fresh
// ConsumerEphemeralPub, runs Balance + BrokerRequest, and maps the
// SeederAssignment into the driver RequestResult shape; a pushed settlement
// is counter-signed with the IDENTITY key and delivered via Settle.
func TestConsumerActor_RequestBrokerSettlementFlow(t *testing.T) {
	fx := startConsumerActor(t, false)
	const model = "claude-sonnet-4-6"

	// dial=false: the assignment's SeederAddr is fake; the tunnel data-plane
	// is covered by the Docker scenario. settle stays default (true).
	status := ctlPostJSON(t, fx.base+"/config", []byte(`{"dial":false}`), nil)
	require.Less(t, status, 300, "POST /config must succeed")

	var res struct {
		Outcome             string `json:"outcome"`
		SeederAddr          string `json:"seeder_addr"`
		SeederPubkeyHex     string `json:"seeder_pubkey_hex"`
		ReservationTokenHex string `json:"reservation_token_hex"`
		ResponseBody        string `json:"response_body"`
		Error               string `json:"error"`
	}
	status = ctlPostJSON(t, fx.base+"/request",
		[]byte(`{"model":"`+model+`","max_input_tokens":4096,"max_output_tokens":1024}`), &res)
	require.Equal(t, http.StatusOK, status)

	asg := testAssignment()
	assert.Equal(t, "seeder_assignment", res.Outcome)
	assert.Equal(t, string(asg.SeederAddr), res.SeederAddr)
	assert.Equal(t, hex.EncodeToString(asg.SeederPubkey), res.SeederPubkeyHex)
	assert.Equal(t, hex.EncodeToString(asg.ReservationToken), res.ReservationTokenHex)
	assert.Empty(t, res.ResponseBody, "dial=false must skip the tunnel")
	assert.Empty(t, res.Error)

	// Balance was looked up under the ENROLL-returned id.
	select {
	case id := <-fx.balanceIDs:
		assert.Equal(t, fx.enrollID, id, "Balance lookup key must be the enroll id (SPKI hash), not sha256(rawPubkey)")
	case <-time.After(2 * time.Second):
		t.Fatal("no Balance RPC observed")
	}

	// The envelope must be valid, carry the enroll id as ConsumerId, embed a
	// 32-byte ephemeral pub, and verify under the actor's identity pubkey.
	var env *tbproto.EnvelopeSigned
	select {
	case env = <-fx.envelopes:
	case <-time.After(2 * time.Second):
		t.Fatal("no BrokerRequest envelope observed")
	}
	require.NoError(t, tbproto.ValidateEnvelopeBody(env.Body))
	assert.Equal(t, fx.enrollID, env.Body.ConsumerId,
		"envelope ConsumerId must be the enroll-returned id, not sha256(rawPubkey)")
	assert.Len(t, env.Body.ConsumerEphemeralPub, 32, "envelope must carry a fresh consumer ephemeral pub")
	assert.Equal(t, model, env.Body.Model)
	assert.Equal(t, uint64(4096), env.Body.MaxInputTokens)
	assert.Equal(t, uint64(1024), env.Body.MaxOutputTokens)
	assert.True(t, signing.VerifyEnvelope(fx.pubkey, env),
		"envelope sig must verify under the actor's identity pubkey")

	// Push a settlement: the actor must counter-sign the preimage with the
	// IDENTITY key and deliver it via the Settle RPC, then ack the push.
	preimage, hash, requestID, err := pushSettlementFor(t, fx, model)
	require.NoError(t, err, "settlement push must be acked when settle=true")

	var settle *tbproto.SettleRequest
	select {
	case settle = <-fx.settles:
	case <-time.After(2 * time.Second):
		t.Fatal("no Settle RPC observed after the settlement push")
	}
	assert.Equal(t, hash[:], settle.PreimageHash)
	assert.True(t, ed25519.Verify(fx.pubkey, preimage, settle.ConsumerSig),
		"counter-sig must be the IDENTITY key over the raw preimage bytes")

	// /settlement/last reflects the counter-signed settlement.
	var info struct {
		RequestIDHex string `json:"request_id_hex"`
		Signed       bool   `json:"signed"`
		SettledAt    string `json:"settled_at"`
	}
	require.Eventually(t, func() bool {
		ctlGetJSON(t, fx.base+"/settlement/last", &info)
		return info.RequestIDHex != ""
	}, 2*time.Second, 20*time.Millisecond, "/settlement/last never populated")
	assert.Equal(t, hex.EncodeToString(requestID), info.RequestIDHex)
	assert.True(t, info.Signed)
	assert.NotEmpty(t, info.SettledAt)

	// GET /balance surfaces the tracker's snapshot (driver ActorBalance shape).
	var bal struct {
		Credits     int64  `json:"credits"`
		ChainTipSeq uint64 `json:"chain_tip_seq"`
		IssuedAt    uint64 `json:"issued_at"`
		ExpiresAt   uint64 `json:"expires_at"`
	}
	ctlGetJSON(t, fx.base+"/balance", &bal)
	assert.Equal(t, int64(1_000_000), bal.Credits)
	assert.Equal(t, uint64(42), bal.ChainTipSeq)
	assert.NotZero(t, bal.IssuedAt)
	assert.NotZero(t, bal.ExpiresAt)
}

// TestConsumerActor_SettleFalseSuppressesCountersign asserts the dispute
// toggle: with settle=false the SettlementHandler refuses — no Settle RPC,
// no SettleAck on the push stream — and /settlement/last records signed=false.
func TestConsumerActor_SettleFalseSuppressesCountersign(t *testing.T) {
	fx := startConsumerActor(t, false)
	const model = "claude-sonnet-4-6"

	status := ctlPostJSON(t, fx.base+"/config", []byte(`{"settle":false,"dial":false}`), nil)
	require.Less(t, status, 300)

	_, _, requestID, err := pushSettlementFor(t, fx, model)
	assert.Error(t, err, "push must NOT be acked when settle=false (tracker records a dispute)")

	select {
	case sr := <-fx.settles:
		t.Fatalf("unexpected Settle RPC with settle=false: %v", sr)
	case <-time.After(200 * time.Millisecond):
	}

	var info struct {
		RequestIDHex string `json:"request_id_hex"`
		Signed       bool   `json:"signed"`
	}
	ctlGetJSON(t, fx.base+"/settlement/last", &info)
	assert.Equal(t, hex.EncodeToString(requestID), info.RequestIDHex)
	assert.False(t, info.Signed)
}

// TestConsumerActor_TransferAgainstTrackerB drives POST /transfer: the actor
// opens a second trackerclient to tracker B and sends a TransferRequest whose
// consumer signature covers the federation-canonical TransferProofRequest
// built from the source/dest FEDERATION tracker_ids and the ENROLL id.
func TestConsumerActor_TransferAgainstTrackerB(t *testing.T) {
	fx := startConsumerActor(t, true)

	var res struct {
		SourceChainTipHashHex string `json:"source_chain_tip_hash_hex"`
		SourceSeq             uint64 `json:"source_seq"`
		Error                 string `json:"error"`
	}
	status := ctlPostJSON(t, fx.base+"/transfer", []byte(`{"amount":250,"dest_region":"B"}`), &res)
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, res.Error)
	assert.Equal(t, uint64(7), res.SourceSeq)
	assert.Equal(t, hex.EncodeToString(bytes.Repeat([]byte{0x77}, 32)), res.SourceChainTipHashHex)

	var tr *tbproto.TransferRequest
	select {
	case tr = <-fx.transfersB:
	case <-time.After(2 * time.Second):
		t.Fatal("no TransferRequest RPC observed on tracker B")
	}
	assert.Equal(t, fx.enrollID, tr.IdentityId, "transfer identity must be the enroll id")
	assert.Equal(t, uint64(250), tr.Amount)
	assert.Equal(t, "B", tr.DestRegion)
	assert.Equal(t, testSourceFedID[:], tr.SourceTrackerId)
	assert.Len(t, tr.Nonce, 32)
	assert.Equal(t, []byte(fx.pubkey), tr.ConsumerPub)

	// The sig must cover the federation-canonical TransferProofRequest,
	// including the DEST federation tracker_id (not on the tbproto wire).
	canonical, err := fed.CanonicalTransferProofRequestPreSig(&fed.TransferProofRequest{
		SourceTrackerId: testSourceFedID[:],
		DestTrackerId:   testDestFedID[:],
		IdentityId:      tr.IdentityId,
		Amount:          tr.Amount,
		Nonce:           tr.Nonce,
		ConsumerPub:     tr.ConsumerPub,
		Timestamp:       tr.Timestamp,
	})
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(fx.pubkey, canonical, tr.ConsumerSig),
		"consumer sig must verify over CanonicalTransferProofRequestPreSig with the dest fed-id included")
}
