package broker

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestNewBrokerMetrics_FieldsPopulated(t *testing.T) {
	m := newBrokerMetrics()
	require.NotNil(t, m)

	// Hot-path
	require.NotNil(t, m.SubmitDecisions)
	require.NotNil(t, m.SubmitDuration)
	require.NotNil(t, m.OfferAttempts)
	require.NotNil(t, m.InflightCount)

	// Reservations
	require.NotNil(t, m.ReservationsActive)
	require.NotNil(t, m.ReservationsActiveCount)
	require.NotNil(t, m.ReservationTTLExpired)

	// Settlement
	require.NotNil(t, m.SettlementDecisions)
	require.NotNil(t, m.SettlementDuration)
	require.NotNil(t, m.ConsumerSigMissing)
	require.NotNil(t, m.LedgerAppendFailure)
	require.NotNil(t, m.StaleTipRetries)

	// Operational
	require.NotNil(t, m.QueueDrainPops)
	require.NotNil(t, m.QueueDrainAdmitOutcomes)
	require.NotNil(t, m.SeederPostAcceptDisconnect)
}

func TestNewBrokerMetrics_RegistersWithRegistry(t *testing.T) {
	m := newBrokerMetrics()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(m.Collector()))

	// Use Describe to enumerate metric descriptors — this works even for
	// CounterVec / HistogramVec whose families only appear in Gather after
	// at least one observation.
	descCh := make(chan *prometheus.Desc, 64)
	m.Collector().Describe(descCh)
	close(descCh)

	want := map[string]bool{
		"broker_submit_decisions_total":              false,
		"broker_submit_duration_seconds":             false,
		"broker_offer_attempts_total":                false,
		"broker_inflight_count":                      false,
		"broker_reservations_active":                 false,
		"broker_reservations_active_count":           false,
		"broker_reservation_ttl_expired_total":       false,
		"broker_settlement_decisions_total":          false,
		"broker_settlement_duration_seconds":         false,
		"broker_consumer_sig_missing_total":          false,
		"broker_ledger_append_failure_total":         false,
		"broker_stale_tip_retries_total":             false,
		"broker_queue_drain_pops_total":              false,
		"broker_queue_drain_admit_outcomes_total":    false,
		"broker_seeder_post_accept_disconnect_total": false,
	}
	for desc := range descCh {
		// prometheus.Desc.String() encodes the fully-qualified name as:
		//   Desc{fqName: "...", ...}
		s := desc.String()
		for name := range want {
			if strings.Contains(s, `"`+name+`"`) {
				want[name] = true
			}
		}
	}
	for name, found := range want {
		require.True(t, found, "metric descriptor %q not found via Describe", name)
	}
}
