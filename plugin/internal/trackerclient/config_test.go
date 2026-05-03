package trackerclient

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

type fakeSigner struct{ priv ed25519.PrivateKey }

func (f fakeSigner) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(f.priv, msg), nil }
func (f fakeSigner) PrivateKey() ed25519.PrivateKey  { return f.priv }
func (f fakeSigner) IdentityID() ids.IdentityID      { return ids.IdentityID{} }

func validConfig(t *testing.T) Config {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return Config{
		Endpoints: []TrackerEndpoint{{
			Addr:         "127.0.0.1:9000",
			IdentityHash: [32]byte{1},
		}},
		Identity: fakeSigner{priv: priv},
	}
}

func TestConfigValidateAccepts(t *testing.T) {
	cfg := validConfig(t).withDefaults()
	require.NoError(t, cfg.Validate())
}

func TestConfigValidateRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		target error
	}{
		{"no endpoints", func(c *Config) { c.Endpoints = nil }, ErrInvalidEndpoint},
		{"empty addr", func(c *Config) { c.Endpoints[0].Addr = "" }, ErrInvalidEndpoint},
		{"zero hash", func(c *Config) { c.Endpoints[0].IdentityHash = [32]byte{} }, ErrInvalidEndpoint},
		{"no identity", func(c *Config) { c.Identity = nil }, ErrConfigInvalid},
		{"zero misses", func(c *Config) { c.HeartbeatMisses = 0 /* defaults bypass */ }, nil},
		{"refresh ≥ ttl", func(c *Config) {
			c.BalanceTTL = time.Minute
			c.BalanceRefreshHeadroom = time.Hour
		}, ErrConfigInvalid},
		{"backoff inverted", func(c *Config) {
			c.BackoffBase = 30 * time.Second
			c.BackoffMax = 1 * time.Second
		}, ErrConfigInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig(t)
			c.mutate(&cfg)
			err := cfg.withDefaults().Validate()
			if c.target == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, c.target), "want errors.Is %v, got %v", c.target, err)
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	assert.Equal(t, 5*time.Second, cfg.DialTimeout)
	assert.Equal(t, 15*time.Second, cfg.HeartbeatPeriod)
	assert.Equal(t, 3, cfg.HeartbeatMisses)
	assert.Equal(t, 10*time.Minute, cfg.BalanceTTL)
	assert.Equal(t, 2*time.Minute, cfg.BalanceRefreshHeadroom)
	assert.Equal(t, 200*time.Millisecond, cfg.BackoffBase)
	assert.Equal(t, 30*time.Second, cfg.BackoffMax)
	assert.Equal(t, 1<<20, cfg.MaxFrameSize)
}
