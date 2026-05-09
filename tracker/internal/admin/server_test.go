package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// stubPeerCounter implements PeerCounter.
type stubPeerCounter struct{ n int }

func (s stubPeerCounter) PeerCount() int { return s.n }

// stubLedger implements LedgerView.
type stubLedger struct {
	tipSeq    uint64
	tipHash   []byte
	hasTip    bool
	tipErr    error
	balance   *tbproto.SignedBalanceSnapshot
	balanceBy map[[32]byte]*tbproto.SignedBalanceSnapshot
	balErr    error
}

func (s *stubLedger) Tip(_ context.Context) (uint64, []byte, bool, error) {
	return s.tipSeq, s.tipHash, s.hasTip, s.tipErr
}

func (s *stubLedger) SignedBalance(_ context.Context, id []byte) (*tbproto.SignedBalanceSnapshot, error) {
	if s.balErr != nil {
		return nil, s.balErr
	}
	if s.balanceBy != nil {
		var key [32]byte
		copy(key[:], id)
		if b, ok := s.balanceBy[key]; ok {
			return b, nil
		}
		return nil, nil
	}
	return s.balance, nil
}

// stubRegistry implements RegistryView.
type stubRegistry struct {
	by map[ids.IdentityID]registry.SeederRecord
}

func (s *stubRegistry) Get(id ids.IdentityID) (registry.SeederRecord, bool) {
	rec, ok := s.by[id]
	return rec, ok
}

func newTestDeps() Deps {
	cfg := config.DefaultConfig()
	cfg.Admin.ListenAddr = "127.0.0.1:0"
	cfg.Ledger.MerkleRootIntervalMin = 60
	return Deps{
		Config:             cfg,
		Logger:             zerolog.Nop(),
		Now:                func() time.Time { return time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC) },
		Version:            "test",
		TokenValidator:     func(t string) bool { return t == "test-token" },
		PeerCounter:        stubPeerCounter{n: 7},
		Registry:           &stubRegistry{by: map[ids.IdentityID]registry.SeederRecord{}},
		Ledger:             &stubLedger{},
		TriggerMaintenance: func() {},
	}
}

func newTestServer(t *testing.T, mutate func(*Deps)) *httptest.Server {
	t.Helper()
	d := newTestDeps()
	if mutate != nil {
		mutate(&d)
	}
	srv, err := New(d)
	require.NoError(t, err)
	return httptest.NewServer(srv.Handler())
}

func adminReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader = http.NoBody
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestNew_RequiresFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"nil config", func(d *Deps) { d.Config = nil }},
		{"empty listen", func(d *Deps) { d.Config.Admin.ListenAddr = "" }},
		{"nil validator", func(d *Deps) { d.TokenValidator = nil }},
		{"nil counter", func(d *Deps) { d.PeerCounter = nil }},
		{"nil registry", func(d *Deps) { d.Registry = nil }},
		{"nil ledger", func(d *Deps) { d.Ledger = nil }},
		{"nil trigger", func(d *Deps) { d.TriggerMaintenance = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newTestDeps()
			c.mutate(&d)
			_, err := New(d)
			assert.Error(t, err)
		})
	}
}

func TestRunShutdown_Idempotent(t *testing.T) {
	d := newTestDeps()
	d.Config.Admin.ListenAddr = "127.0.0.1:0"
	srv, err := New(d)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Wait for listener to bind.
	require.Eventually(t, func() bool { return srv.ListenAddr() != "" }, time.Second, 10*time.Millisecond)

	// Second Run while running returns ErrAlreadyRunning.
	err2 := srv.Run(context.Background())
	assert.ErrorIs(t, err2, ErrAlreadyRunning)

	// Shutdown drains.
	shutCtx, sc := context.WithTimeout(context.Background(), 2*time.Second)
	defer sc()
	require.NoError(t, srv.Shutdown(shutCtx))

	// Second Shutdown is a no-op.
	require.NoError(t, srv.Shutdown(shutCtx))

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

func TestShutdownBeforeRun(t *testing.T) {
	d := newTestDeps()
	srv, err := New(d)
	require.NoError(t, err)
	assert.NoError(t, srv.Shutdown(context.Background()))
}
