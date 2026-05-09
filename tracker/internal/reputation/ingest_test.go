package reputation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/admission"
)

func countEvents(t *testing.T, s *Subsystem, kind SignalKind) int {
	t.Helper()
	var n int
	require.NoError(t, s.store.db.QueryRow(
		`SELECT count(*) FROM rep_events WHERE event_type = ?`,
		int(kind)).Scan(&n))
	return n
}

func TestIngest_RecordBrokerRequest(t *testing.T) {
	s := openForTest(t)
	ctx := context.Background()
	require.NoError(t, s.RecordBrokerRequest(mkID(0x01), "admit"))

	require.Equal(t, 1, countEvents(t, s, SignalBrokerRequest))
	row, ok, err := s.store.readState(ctx, mkID(0x01))
	require.NoError(t, err)
	require.True(t, ok, "ingest must INSERT OR IGNORE rep_state")
	require.Equal(t, StateOK, row.State)
}

func TestIngest_RecordOfferOutcome(t *testing.T) {
	s := openForTest(t)
	require.NoError(t, s.RecordOfferOutcome(mkID(0x02), "accept"))
	require.NoError(t, s.RecordOfferOutcome(mkID(0x02), "reject"))
	require.NoError(t, s.RecordOfferOutcome(mkID(0x02), "unreachable"))

	require.Equal(t, 1, countEvents(t, s, SignalOfferAccept))
	require.Equal(t, 1, countEvents(t, s, SignalOfferReject))
	require.Equal(t, 1, countEvents(t, s, SignalOfferUnreachable))
}

func TestIngest_OnLedgerEvent_Settlement(t *testing.T) {
	s := openForTest(t)
	consumer := mkID(0xC0)
	seeder := mkID(0x5E)
	s.OnLedgerEvent(admission.LedgerEvent{
		Kind:        admission.LedgerEventSettlement,
		ConsumerID:  consumer,
		SeederID:    seeder,
		CostCredits: 12345,
		Flags:       0,
		Timestamp:   time.Unix(1_700_000_000, 0),
	})

	require.Equal(t, 1, countEvents(t, s, SignalSettlementClean))
	require.Equal(t, 0, countEvents(t, s, SignalSettlementSigMissing))
}

func TestIngest_OnLedgerEvent_SettlementSigMissing(t *testing.T) {
	s := openForTest(t)
	s.OnLedgerEvent(admission.LedgerEvent{
		Kind:        admission.LedgerEventSettlement,
		ConsumerID:  mkID(0xC1),
		SeederID:    mkID(0x5F),
		CostCredits: 1,
		Flags:       1, // bit 0 = consumer_sig_missing
		Timestamp:   time.Unix(1, 0),
	})
	require.Equal(t, 0, countEvents(t, s, SignalSettlementClean))
	require.Equal(t, 1, countEvents(t, s, SignalSettlementSigMissing))
}

func TestIngest_OnLedgerEvent_DisputeFiled(t *testing.T) {
	s := openForTest(t)
	s.OnLedgerEvent(admission.LedgerEvent{
		Kind:       admission.LedgerEventDisputeFiled,
		ConsumerID: mkID(0xC2),
		Timestamp:  time.Unix(1, 0),
	})
	require.Equal(t, 1, countEvents(t, s, SignalDisputeFiled))
}

func TestIngest_OnLedgerEvent_DisputeUpheld(t *testing.T) {
	s := openForTest(t)
	s.OnLedgerEvent(admission.LedgerEvent{
		Kind:          admission.LedgerEventDisputeResolved,
		ConsumerID:    mkID(0xC3),
		DisputeUpheld: true,
		Timestamp:     time.Unix(1, 0),
	})
	require.Equal(t, 1, countEvents(t, s, SignalDisputeUpheld))
}

func TestIngest_OnLedgerEvent_Settlement_EnsuresConsumerState(t *testing.T) {
	s := openForTest(t)
	ctx := context.Background()
	consumer := mkID(0xCA)
	seeder := mkID(0x5A)
	s.OnLedgerEvent(admission.LedgerEvent{
		Kind:        admission.LedgerEventSettlement,
		ConsumerID:  consumer,
		SeederID:    seeder,
		CostCredits: 100,
		Timestamp:   time.Unix(1_700_000_000, 0),
	})

	// Consumer must have a rep_state row even though no signal is
	// appended for the consumer side. first_seen_at drives the
	// longevity bonus.
	_, ok, err := s.store.readState(ctx, consumer)
	require.NoError(t, err)
	require.True(t, ok, "OnLedgerEvent settlement must ensureState for ConsumerID")
}

func TestIngest_AfterCloseReturnsSentinel(t *testing.T) {
	s := openForTest(t)
	require.NoError(t, s.Close())
	require.ErrorIs(t, s.RecordBrokerRequest(mkID(0x01), "admit"),
		ErrSubsystemClosed)
	require.ErrorIs(t, s.RecordOfferOutcome(mkID(0x02), "accept"),
		ErrSubsystemClosed)
	// OnLedgerEvent silently no-ops after close (no error return).
	s.OnLedgerEvent(admission.LedgerEvent{Kind: admission.LedgerEventSettlement})
}
