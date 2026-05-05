package trackerclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

type noopTransport struct{}

func (noopTransport) Dial(_ context.Context, _ transport.Endpoint, _ transport.Identity) (transport.Conn, error) {
	return nil, errors.New("no")
}

func TestNewRejectsBadConfig(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err)
}

func TestNewAcceptsValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.Transport = noopTransport{}
	_, err := New(cfg)
	require.NoError(t, err)
}

func TestStartTwiceErrors(t *testing.T) {
	cfg := validConfig(t)
	cfg.Transport = noopTransport{}
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))
	err = c.Start(context.Background())
	assert.ErrorIs(t, err, ErrAlreadyStarted)
	require.NoError(t, c.Close())
}

func TestCloseBeforeStart(t *testing.T) {
	cfg := validConfig(t)
	cfg.Transport = noopTransport{}
	c, err := New(cfg)
	require.NoError(t, err)
	assert.NoError(t, c.Close())
}

func TestWaitConnectedTimesOut(t *testing.T) {
	cfg := validConfig(t)
	cfg.Transport = noopTransport{}
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = c.WaitConnected(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
