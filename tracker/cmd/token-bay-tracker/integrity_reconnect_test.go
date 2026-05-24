package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

func TestRunReconnectIntegrityCheck_PassRecordsCounter(t *testing.T) {
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

	depeerCalls := 0
	depeer := func(_ ids.TrackerID, _ federation.DepeerReason) error {
		depeerCalls++
		return nil
	}

	peer := ids.TrackerID{0x42}
	runReconnectIntegrityCheck(context.Background(), l, m, zerolog.New(io.Discard), peer, depeer)

	assert.Equal(t, float64(1), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")))
	assert.Equal(t, 0, depeerCalls, "depeer must not fire on a healthy chain")
}

func TestRunReconnectIntegrityCheck_FailDropsPeer(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()

	l1, store1, priv := openTestLedger(t, dbPath)
	identity := bytes.Repeat([]byte{0x11}, 32)
	for range 3 {
		_, err := l1.IssueStarterGrant(ctx, identity, 100)
		require.NoError(t, err)
	}
	require.NoError(t, l1.Close())
	require.NoError(t, store1.Close())

	// Tamper with seq=2's canonical entry blob.
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

	store2, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	l2, err := ledger.Open(store2, priv)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	m := ledger.NewIntegrityMetrics(reg)

	var depeeredPeer ids.TrackerID
	var depeeredReason federation.DepeerReason
	depeerCalls := 0
	depeer := func(id ids.TrackerID, r federation.DepeerReason) error {
		depeeredPeer = id
		depeeredReason = r
		depeerCalls++
		return nil
	}

	peer := ids.TrackerID{0xAA}
	runReconnectIntegrityCheck(ctx, l2, m, zerolog.New(io.Discard), peer, depeer)

	assert.Equal(t, float64(1), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("fail")))
	assert.Equal(t, float64(0), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")))
	assert.Equal(t, 1, depeerCalls)
	assert.Equal(t, peer, depeeredPeer)
	assert.Equal(t, federation.ReasonLocalChainCorrupt, depeeredReason)
}

// Spec test: race-clean with concurrent reconnects. The counter must
// remain atomic and report the exact number of pass results.
func TestRunReconnectIntegrityCheck_ConcurrentReconnectsAreRaceClean(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	l, store, _ := openTestLedger(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	identity := bytes.Repeat([]byte{0x11}, 32)
	for range 5 {
		_, err := l.IssueStarterGrant(context.Background(), identity, 100)
		require.NoError(t, err)
	}

	reg := prometheus.NewRegistry()
	m := ledger.NewIntegrityMetrics(reg)

	var depeerCalls atomic.Int32
	depeer := func(_ ids.TrackerID, _ federation.DepeerReason) error {
		depeerCalls.Add(1)
		return nil
	}

	const N = 50
	var wg sync.WaitGroup
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer := ids.TrackerID{byte(i)}
			runReconnectIntegrityCheck(context.Background(), l, m, zerolog.New(io.Discard), peer, depeer)
		}()
	}
	wg.Wait()

	assert.Equal(t, float64(N), testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")))
	assert.Equal(t, int32(0), depeerCalls.Load())
}
