package seederflow_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

func TestActiveClient_InactiveByDefault(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.False(t, c.IsActive(pub))
	require.False(t, c.IsActiveByHash(ccbridge.ClientHash(pub)))
}

func TestActiveClient_RegisterAndUnregister(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	c.RegisterActive(pub)
	require.True(t, c.IsActive(pub))
	require.True(t, c.IsActiveByHash(ccbridge.ClientHash(pub)))

	c.UnregisterActive(pub)
	require.False(t, c.IsActive(pub))
	require.False(t, c.IsActiveByHash(ccbridge.ClientHash(pub)))
}

func TestActiveClient_RefcountTwoConcurrent(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	c.RegisterActive(pub)
	c.RegisterActive(pub) // simulate two concurrent serves

	c.UnregisterActive(pub)
	require.True(t, c.IsActive(pub), "should remain active until last serve unregisters")

	c.UnregisterActive(pub)
	require.False(t, c.IsActive(pub))
}
