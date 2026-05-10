package seederflow_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// validConfig returns a Config with every required field populated by
// minimal stubs so individual tests can mutate one field at a time.
func validConfig(t *testing.T) seederflow.Config {
	t.Helper()
	return seederflow.Config{
		Logger:         zerolog.Nop(),
		Bridge:         &stubBridge{},
		Tracker:        &stubTracker{},
		AuditLog:       &stubAuditLog{},
		Signer:         newStubSigner(t),
		Acceptor:       &stubAcceptor{},
		Runner:         &stubRunner{},
		ConformanceFn:  func(context.Context, ccbridge.Runner) error { return nil },
		IdlePolicy:     seederflow.IdlePolicy{Mode: seederflow.IdleAlwaysOn},
		ActivityGrace:  10 * time.Minute,
		HeadroomWindow: 15 * time.Minute,
		Models:         []string{"claude-sonnet-4-6"},
	}
}

func TestNew_RejectsZeroConfig(t *testing.T) {
	_, err := seederflow.New(seederflow.Config{})
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsMissingBridge(t *testing.T) {
	cfg := validConfig(t)
	cfg.Bridge = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsMissingTracker(t *testing.T) {
	cfg := validConfig(t)
	cfg.Tracker = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsMissingAcceptor(t *testing.T) {
	cfg := validConfig(t)
	cfg.Acceptor = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsMissingSigner(t *testing.T) {
	cfg := validConfig(t)
	cfg.Signer = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsMissingAuditLog(t *testing.T) {
	cfg := validConfig(t)
	cfg.AuditLog = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_RejectsEmptyModels(t *testing.T) {
	cfg := validConfig(t)
	cfg.Models = nil
	_, err := seederflow.New(cfg)
	require.ErrorIs(t, err, seederflow.ErrInvalidConfig)
}

func TestNew_HappyPath(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	require.NotNil(t, c)
}

// ---- stubs ----

type stubBridge struct {
	usage ccbridge.Usage
	err   error
	calls int
}

func (s *stubBridge) Serve(_ context.Context, _ ccbridge.Request, sink io.Writer) (ccbridge.Usage, error) {
	s.calls++
	if s.err != nil {
		return ccbridge.Usage{}, s.err
	}
	if sink != nil {
		_, _ = sink.Write([]byte(`{"type":"system","subtype":"init"}` + "\n"))
		_, _ = sink.Write([]byte(`{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"))
	}
	if s.usage == (ccbridge.Usage{}) {
		s.usage = ccbridge.Usage{InputTokens: 10, OutputTokens: 20}
	}
	return s.usage, nil
}

type stubTracker struct {
	mu              sync.Mutex
	usageReportsBg  []*trackerclient.UsageReport
	advertismentsBg []*trackerclient.Advertisement
	usageErr        error
}

func (s *stubTracker) UsageReport(_ context.Context, ur *trackerclient.UsageReport) error {
	s.mu.Lock()
	s.usageReportsBg = append(s.usageReportsBg, ur)
	s.mu.Unlock()
	return s.usageErr
}

func (s *stubTracker) Advertise(_ context.Context, ad *trackerclient.Advertisement) error {
	s.mu.Lock()
	s.advertismentsBg = append(s.advertismentsBg, ad)
	s.mu.Unlock()
	return nil
}

func (s *stubTracker) UsageReports() []*trackerclient.UsageReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*trackerclient.UsageReport, len(s.usageReportsBg))
	copy(out, s.usageReportsBg)
	return out
}

func (s *stubTracker) Advertisements() []*trackerclient.Advertisement {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*trackerclient.Advertisement, len(s.advertismentsBg))
	copy(out, s.advertismentsBg)
	return out
}

type stubAuditLog struct {
	mu        sync.Mutex
	recordsBg []auditlog.SeederRecord
}

func (s *stubAuditLog) LogSeeder(rec auditlog.SeederRecord) error {
	s.mu.Lock()
	s.recordsBg = append(s.recordsBg, rec)
	s.mu.Unlock()
	return nil
}

func (s *stubAuditLog) Records() []auditlog.SeederRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]auditlog.SeederRecord, len(s.recordsBg))
	copy(out, s.recordsBg)
	return out
}

type stubSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newStubSigner(t *testing.T) *stubSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &stubSigner{priv: priv, pub: pub}
}

func (s *stubSigner) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, msg), nil
}
func (s *stubSigner) PublicKey() ed25519.PublicKey { return s.pub }

type stubRunner struct{}

func (stubRunner) Run(context.Context, ccbridge.Request, io.Writer) error { return nil }

type stubAcceptor struct {
	addr     netip.AddrPort
	conns    chan seederflow.TunnelConn
	closed   bool
	closeErr error
}

func newStubAcceptor() *stubAcceptor {
	return &stubAcceptor{
		addr:  netip.MustParseAddrPort("127.0.0.1:1"),
		conns: make(chan seederflow.TunnelConn, 4),
	}
}

func (s *stubAcceptor) Accept(ctx context.Context) (seederflow.TunnelConn, error) {
	select {
	case c, ok := <-s.conns:
		if !ok {
			return nil, errors.New("acceptor closed")
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *stubAcceptor) LocalAddr() netip.AddrPort { return s.addr }
func (s *stubAcceptor) Close() error {
	if !s.closed {
		s.closed = true
		close(s.conns)
	}
	return s.closeErr
}
