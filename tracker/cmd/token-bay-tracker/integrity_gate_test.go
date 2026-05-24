package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

func newTestTrackerKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)
	return priv
}

func openTestLedger(t *testing.T, dbPath string) (*ledger.Ledger, *storage.Store, ed25519.PrivateKey) {
	t.Helper()
	priv := newTestTrackerKey(t)
	store, err := storage.Open(context.Background(), dbPath)
	require.NoError(t, err)
	l, err := ledger.Open(store, priv)
	require.NoError(t, err)
	return l, store, priv
}

func TestRunStartupIntegrityCheck_PassesOnCleanChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	l, store, _ := openTestLedger(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	identity := bytes.Repeat([]byte{0x11}, 32)
	for range 3 {
		_, err := l.IssueStarterGrant(context.Background(), identity, 100)
		require.NoError(t, err)
	}

	reg := prometheus.NewRegistry()
	m := ledger.NewIntegrityMetrics(reg)

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	err := runStartupIntegrityCheck(context.Background(), l, m, logger)
	require.NoError(t, err)

	assert.Equal(t, float64(1), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("fail")))
	assert.Contains(t, logBuf.String(), "ledger integrity")
}

func TestRunStartupIntegrityCheck_FailsOnTamperedChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()

	// Phase 1: build a real 3-entry chain.
	l1, store1, priv := openTestLedger(t, dbPath)
	identity := bytes.Repeat([]byte{0x11}, 32)
	for range 3 {
		_, err := l1.IssueStarterGrant(ctx, identity, 100)
		require.NoError(t, err)
	}
	require.NoError(t, l1.Close())
	require.NoError(t, store1.Close())

	// Phase 2: tamper with entry seq=2's canonical blob.
	rawDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	var origCanonical []byte
	require.NoError(t, rawDB.QueryRowContext(ctx, "SELECT canonical FROM entries WHERE seq = 2").Scan(&origCanonical))
	body := &tbproto.EntryBody{}
	require.NoError(t, proto.Unmarshal(origCanonical, body))
	body.PrevHash = bytes.Repeat([]byte{0xFF}, 32)
	tampered, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)
	_, err = rawDB.ExecContext(ctx, "UPDATE entries SET canonical = ? WHERE seq = 2", tampered)
	require.NoError(t, err)
	require.NoError(t, rawDB.Close())

	// Phase 3: reopen via the same path the production composition root uses.
	store2, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	l2, err := ledger.Open(store2, priv)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	m := ledger.NewIntegrityMetrics(reg)

	err = runStartupIntegrityCheck(ctx, l2, m, zerolog.New(io.Discard))
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "chain break")

	assert.Equal(t, float64(0), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("fail")))
}
