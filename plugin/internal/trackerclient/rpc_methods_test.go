package trackerclient

import (
	"context"
	"crypto/ed25519"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// newWiredClient stands up a wired Client + fakeserver pair using loopback.
func newWiredClient(t *testing.T, register func(*fakeserver.Server)) (*Client, func()) {
	return newWiredClientWithSigner(t, nil, register)
}

// newWiredClientWithSigner is like newWiredClient but lets the caller
// inject a specific Ed25519 priv key so the test can verify signatures
// produced by the Client against the matching pubkey. priv == nil falls
// back to the default randomly-generated key in validConfig.
func newWiredClientWithSigner(t *testing.T, priv ed25519.PrivateKey, register func(*fakeserver.Server)) (*Client, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	if register != nil {
		register(fake)
	}

	cfg := validConfig(t)
	if priv != nil {
		cfg.Identity = fakeSigner{priv: priv}
	}
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))

	serverDone := make(chan struct{})
	go func() {
		_ = fake.Run(context.Background())
		close(serverDone)
	}()

	_ = cli // loopback driver returns srv.peer (which is cli) on Dial.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))

	return c, func() {
		_ = c.Close()
		_ = srv.Close()
		<-serverDone
	}
}

func TestBrokerRequest_Assignment(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerRequestResponse{
				Outcome: &tbproto.BrokerRequestResponse_SeederAssignment{
					SeederAssignment: &tbproto.SeederAssignment{
						SeederAddr:       []byte("127.0.0.1:5000"),
						SeederPubkey:     make([]byte, 32),
						ReservationToken: make([]byte, 16),
					},
				},
			}, nil
		}
	})
	defer cleanup()

	env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
	res, err := c.BrokerRequest(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, BrokerOutcomeAssignment, res.Outcome)
	require.NotNil(t, res.Assignment)
	assert.Equal(t, "127.0.0.1:5000", res.Assignment.SeederAddr)
	assert.Equal(t, make([]byte, 32), res.Assignment.SeederPubkey)
	assert.Equal(t, make([]byte, 16), res.Assignment.ReservationToken)
}

func TestBrokerRequest_NoCapacity(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerRequestResponse{
				Outcome: &tbproto.BrokerRequestResponse_NoCapacity{
					NoCapacity: &tbproto.NoCapacity{Reason: "all seeders busy"},
				},
			}, nil
		}
	})
	defer cleanup()

	env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
	res, err := c.BrokerRequest(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, BrokerOutcomeNoCapacity, res.Outcome)
	require.NotNil(t, res.NoCap)
	assert.Equal(t, "all seeders busy", res.NoCap.Reason)
}

func TestBrokerRequest_Queued(t *testing.T) {
	rid := make([]byte, 16)
	rid[0] = 0xAB
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerRequestResponse{
				Outcome: &tbproto.BrokerRequestResponse_Queued{
					Queued: &tbproto.Queued{
						RequestId:    rid,
						PositionBand: tbproto.PositionBand_POSITION_BAND_1_TO_10,
						EtaBand:      tbproto.EtaBand_ETA_BAND_LT_30S,
					},
				},
			}, nil
		}
	})
	defer cleanup()

	env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
	res, err := c.BrokerRequest(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, BrokerOutcomeQueued, res.Outcome)
	require.NotNil(t, res.Queued)
	assert.Equal(t, uint8(0xAB), res.Queued.RequestID[0])
	assert.Equal(t, uint8(tbproto.PositionBand_POSITION_BAND_1_TO_10), res.Queued.PositionBand)
	assert.Equal(t, uint8(tbproto.EtaBand_ETA_BAND_LT_30S), res.Queued.EtaBand)
}

func TestBrokerRequest_Rejected(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerRequestResponse{
				Outcome: &tbproto.BrokerRequestResponse_Rejected{
					Rejected: &tbproto.Rejected{
						Reason:      tbproto.RejectReason_REJECT_REASON_REGION_OVERLOADED,
						RetryAfterS: 60,
					},
				},
			}, nil
		}
	})
	defer cleanup()

	env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
	res, err := c.BrokerRequest(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, BrokerOutcomeRejected, res.Outcome)
	require.NotNil(t, res.Rejected)
	assert.Equal(t, uint8(tbproto.RejectReason_REJECT_REASON_REGION_OVERLOADED), res.Rejected.Reason)
	assert.Equal(t, uint32(60), res.Rejected.RetryAfterS)
}

func TestBrokerRequest_NilEnvelope(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()

	_, err := c.BrokerRequest(context.Background(), nil)
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestSettleRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()
	err := c.Settle(context.Background(), make([]byte, 32), make([]byte, 64))
	require.NoError(t, err)
}

func TestSettleRejectsBadHash(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()
	err := c.Settle(context.Background(), []byte{1, 2, 3}, nil)
	require.Error(t, err)
}

func TestBalanceCachedRoundTrip(t *testing.T) {
	id := ids.IdentityID{0xab}
	issued := time.Now().Unix()
	expires := time.Now().Add(10 * time.Minute).Unix()
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BALANCE] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.SignedBalanceSnapshot{
				Body: &tbproto.BalanceSnapshotBody{
					IdentityId: id[:],
					Credits:    100,
					IssuedAt:   uint64(issued),
					ExpiresAt:  uint64(expires),
				},
				TrackerSig: make([]byte, 64),
			}, nil
		}
	})
	defer cleanup()
	snap, err := c.BalanceCached(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, int64(100), snap.Body.Credits)
}

func TestUsageReportRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()
	err := c.UsageReport(context.Background(), &UsageReport{
		RequestID:    uuid.New(),
		InputTokens:  100,
		OutputTokens: 200,
		Model:        "claude-sonnet-4-6",
		SeederSig:    make([]byte, 64),
	})
	require.NoError(t, err)
}

func TestAdvertiseRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()
	err := c.Advertise(context.Background(), &Advertisement{
		Models:     []string{"claude-sonnet-4-6"},
		MaxContext: 200_000,
		Available:  true,
		Headroom:   0.8,
		Tiers:      1,
	})
	require.NoError(t, err)
}

func TestTransferRequestRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TransferProof{
				SourceChainTipHash: make([]byte, 32),
				SourceSeq:          12345,
				TrackerSig:         make([]byte, 64),
			}, nil
		}
	})
	defer cleanup()
	var src, dst [32]byte
	src[0] = 0xAA
	dst[0] = 0xBB
	proof, err := c.TransferRequest(context.Background(), &TransferRequest{
		IdentityID:      ids.IdentityID{1},
		Amount:          50,
		DestRegion:      "us-east-1",
		SourceTrackerID: src,
		DestTrackerID:   dst,
		Timestamp:       1714000000,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(12345), proof.SourceSeq)
}

// TestTransferRequest_PopulatesAllWireFieldsAndSig captures the on-wire
// tbproto.TransferRequest received by the fake server and asserts that
// the plugin populated all 8 fields with a consumer_sig that verifies
// against the federation-canonical bytes the source tracker checks.
func TestTransferRequest_PopulatesAllWireFieldsAndSig(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	var got *tbproto.TransferRequest
	c, cleanup := newWiredClientWithSigner(t, priv, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			got = proto.Clone(req).(*tbproto.TransferRequest)
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TransferProof{
				SourceChainTipHash: make([]byte, 32),
				SourceSeq:          1,
				TrackerSig:         make([]byte, 64),
			}, nil
		}
	})
	defer cleanup()

	var src, dst [32]byte
	src[0] = 0x11
	dst[0] = 0x22
	var nonce [32]byte
	nonce[0] = 0x33
	identity := ids.IdentityID{0x44}

	_, err = c.TransferRequest(context.Background(), &TransferRequest{
		IdentityID:      identity,
		Amount:          250,
		DestRegion:      "us-east-1",
		SourceTrackerID: src,
		DestTrackerID:   dst,
		Nonce:           nonce,
		Timestamp:       1714000000,
	})
	require.NoError(t, err)
	require.NotNil(t, got, "fake server never observed the request")

	assert.Equal(t, identity[:], got.IdentityId)
	assert.Equal(t, uint64(250), got.Amount)
	assert.Equal(t, "us-east-1", got.DestRegion)
	assert.Equal(t, nonce[:], got.Nonce)
	assert.Equal(t, src[:], got.SourceTrackerId)
	assert.Equal(t, []byte(pub), got.ConsumerPub)
	assert.Equal(t, uint64(1714000000), got.Timestamp)
	require.Len(t, got.ConsumerSig, ed25519.SignatureSize)

	// Sig must verify against the federation canonical bytes — what the
	// source tracker actually checks in OnRequest.
	canonical, err := fed.CanonicalTransferProofRequestPreSig(&fed.TransferProofRequest{
		SourceTrackerId: src[:],
		DestTrackerId:   dst[:],
		IdentityId:      identity[:],
		Amount:          250,
		Nonce:           nonce[:],
		ConsumerPub:     pub,
		Timestamp:       1714000000,
	})
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(pub, canonical, got.ConsumerSig),
		"consumer_sig must verify against fed.CanonicalTransferProofRequestPreSig")
}

func TestTransferRequest_MissingSourceTrackerID_Rejected(t *testing.T) {
	c, cleanup := newWiredClient(t, nil)
	defer cleanup()
	_, err := c.TransferRequest(context.Background(), &TransferRequest{
		IdentityID: ids.IdentityID{1},
		Amount:     1,
		Timestamp:  1714000000,
		// SourceTrackerID + DestTrackerID intentionally zero
	})
	require.Error(t, err)
}

func TestStunAllocateRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.StunAllocateResponse{ExternalAddr: "203.0.113.5:51820"}, nil
		}
	})
	defer cleanup()
	addr, err := c.StunAllocate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddrPort("203.0.113.5:51820"), addr)
}

func TestTurnRelayOpenRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.TurnRelayOpenResponse{
				RelayEndpoint: "relay.example:3478",
				Token:         []byte("relay-token"),
			}, nil
		}
	})
	defer cleanup()
	h, err := c.TurnRelayOpen(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, "relay.example:3478", h.Endpoint)
	assert.Equal(t, []byte("relay-token"), h.Token)
}

func TestEnrollRoundTrip(t *testing.T) {
	var got [32]byte
	copy(got[:], "captured-id-12345678901234567890")
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.EnrollResponse{
				IdentityId:          got[:],
				StarterGrantCredits: 50,
				StarterGrantEntry:   []byte("entry-blob"),
			}, nil
		}
	})
	defer cleanup()
	resp, err := c.Enroll(context.Background(), &EnrollRequest{})
	require.NoError(t, err)
	assert.Equal(t, ids.IdentityID(got), resp.IdentityID)
	assert.Equal(t, uint64(50), resp.StarterGrantCredits)
}
