package loopback

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestPairOpenAcceptRoundTrip(t *testing.T) {
	cli, srv := Pair(ids.IdentityID{1}, ids.IdentityID{2})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer s.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(s, buf); err != nil {
			errCh <- err
			return
		}
		if _, err := s.Write(append(buf, '!')); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	s, err := cli.OpenStreamSync(ctx)
	require.NoError(t, err)
	_, err = s.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, s.CloseWrite())

	out := make([]byte, 5)
	_, err = io.ReadFull(s, out)
	require.NoError(t, err)
	assert.Equal(t, []byte("ping!"), out)
	require.NoError(t, <-errCh)
}

func TestCloseRipplesToPeer(t *testing.T) {
	cli, srv := Pair(ids.IdentityID{}, ids.IdentityID{})
	require.NoError(t, cli.Close())
	select {
	case <-srv.Done():
	case <-time.After(time.Second):
		t.Fatal("peer Done channel not closed within 1s")
	}
}

func TestDriverDial(t *testing.T) {
	cli, srv := Pair(ids.IdentityID{1}, ids.IdentityID{2})
	d := NewDriver()
	d.Listen("test:9000", srv)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := d.Dial(ctx, transport.Endpoint{Addr: "test:9000"}, nil)
	require.NoError(t, err)
	_ = got
	_ = cli
}

func TestConn_PeerPublicKey(t *testing.T) {
	clientID := ids.IdentityID{1}
	serverID := ids.IdentityID{2}
	clientPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	serverPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cli, srv := PairWithKeys(clientID, serverID, clientPub, serverPub)
	require.True(t, ed25519.PublicKey(serverPub).Equal(cli.PeerPublicKey()))
	require.True(t, ed25519.PublicKey(clientPub).Equal(srv.PeerPublicKey()))
}
