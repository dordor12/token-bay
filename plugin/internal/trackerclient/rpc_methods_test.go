package trackerclient

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// newWiredClient stands up a wired Client + fakeserver pair using loopback.
func newWiredClient(t *testing.T, register func(*fakeserver.Server)) (*Client, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	if register != nil {
		register(fake)
	}

	cfg := validConfig(t)
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

func TestBrokerRequestRoundTrip(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.BrokerResponse{
				SeederAddr:       []byte("seeder.example:443"),
				SeederPubkey:     make([]byte, 32),
				ReservationToken: []byte("token"),
			}, nil
		}
	})
	defer cleanup()

	env := &tbproto.EnvelopeSigned{Body: &tbproto.EnvelopeBody{}}
	resp, err := c.BrokerRequest(context.Background(), env)
	require.NoError(t, err)
	assert.Equal(t, "seeder.example:443", resp.SeederAddr)
	assert.Equal(t, []byte("token"), resp.ReservationToken)
}

func TestBrokerRequestNoCapacity(t *testing.T) {
	c, cleanup := newWiredClient(t, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY, nil, &tbproto.RpcError{
				Code: "no_capacity", Message: "all seeders busy",
			}
		}
	})
	defer cleanup()

	_, err := c.BrokerRequest(context.Background(), &tbproto.EnvelopeSigned{})
	assert.ErrorIs(t, err, ErrNoCapacity)
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
	proof, err := c.TransferRequest(context.Background(), &TransferRequest{
		IdentityID: ids.IdentityID{1},
		Amount:     50,
		DestRegion: "us-east-1",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(12345), proof.SourceSeq)
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
