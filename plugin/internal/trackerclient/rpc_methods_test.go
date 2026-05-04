package trackerclient

import (
	"context"
	"testing"
	"time"

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
