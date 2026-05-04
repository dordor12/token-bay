package trackerclient

import (
	"context"
	"sync"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	quicdriver "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/quic"
)

// Client is the public surface. Concurrency-safe.
type Client struct {
	cfg    Config
	holder *connHolder
	sup    *supervisor

	startedMu sync.Mutex
	started   bool
	closed    bool

	cache *balanceCache
}

// New validates cfg and constructs (but does not start) a Client.
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	if cfg.Transport == nil {
		cfg.Transport = quicdriver.New()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{
		cfg:    cfg,
		holder: &connHolder{},
		cache:  newBalanceCache(cfg.BalanceRefreshHeadroom, cfg.Clock),
	}, nil
}

// Start launches the supervisor. Returns ErrAlreadyStarted on the second
// call. The supervisor runs until Close is called or ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
	c.startedMu.Lock()
	defer c.startedMu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.started {
		return ErrAlreadyStarted
	}
	c.started = true
	c.sup = newSupervisor(ctx, c.cfg, c.holder)
	c.sup.start()
	return nil
}

// Close terminates the supervisor and is idempotent.
func (c *Client) Close() error {
	c.startedMu.Lock()
	if c.closed {
		c.startedMu.Unlock()
		return nil
	}
	c.closed = true
	sup := c.sup
	c.startedMu.Unlock()
	if sup != nil {
		sup.stop()
	}
	return nil
}

// Status returns a snapshot of supervisor state.
func (c *Client) Status() ConnectionState {
	if c.sup == nil {
		return ConnectionState{Phase: PhaseDisconnected}
	}
	return c.sup.snapshot()
}

// WaitConnected blocks until the supervisor reports Connected, or ctx is done.
func (c *Client) WaitConnected(ctx context.Context) error {
	_, err := c.holder.get(ctx)
	return err
}

// connect returns the current connection or blocks until one exists.
// All RPC methods route through here.
//
//nolint:unused // consumed by RPC methods in Tasks 13-17
func (c *Client) connect(ctx context.Context) (transport.Conn, error) {
	return c.holder.get(ctx)
}
