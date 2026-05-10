package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// captureConn implements PeerConn by recording every frame written via
// Send and blocking Recv until ctx is cancelled. Sufficient for unicast
// tests that don't need a paired connection.
type captureConn struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *captureConn) Send(_ context.Context, frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	dup := make([]byte, len(frame))
	copy(dup, frame)
	c.frames = append(c.frames, dup)
	return nil
}

func (c *captureConn) Recv(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *captureConn) RemoteAddr() string           { return "capture" }
func (c *captureConn) RemotePub() ed25519.PublicKey { return nil }
func (c *captureConn) Close() error                 { return nil }

func TestSendToPeer_HappyPath(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	myID := ids.TrackerID(sha256.Sum256(pub))

	var peerID ids.TrackerID
	peerID[0] = 0x42

	capt := &captureConn{}
	peer := NewPeerForTest(capt, func(*fed.Envelope) {})

	f := &Federation{
		cfg:   (Config{MyTrackerID: myID, MyPriv: priv}).withDefaults(),
		peers: map[ids.TrackerID]*Peer{peerID: peer},
	}

	err = f.SendToPeer(context.Background(), peerID, fed.Kind_KIND_PING, []byte("payload"))
	require.NoError(t, err)

	capt.mu.Lock()
	defer capt.mu.Unlock()
	require.Len(t, capt.frames, 1)

	env, err := UnmarshalFrame(capt.frames[0])
	require.NoError(t, err)
	assert.Equal(t, fed.Kind_KIND_PING, env.Kind)
	assert.Equal(t, []byte("payload"), env.Payload)
	myIDBytes := myID.Bytes()
	assert.Equal(t, myIDBytes[:], env.SenderId)
}

func TestSendToPeer_PeerNotConnected(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	myID := ids.TrackerID(sha256.Sum256(pub))

	f := &Federation{
		cfg:   (Config{MyTrackerID: myID, MyPriv: priv}).withDefaults(),
		peers: map[ids.TrackerID]*Peer{},
	}

	var unknown ids.TrackerID
	for i := range unknown {
		unknown[i] = 0xDD
	}
	err = f.SendToPeer(context.Background(), unknown, fed.Kind_KIND_PING, []byte("x"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerNotConnected), "want ErrPeerNotConnected; got %v", err)
}
