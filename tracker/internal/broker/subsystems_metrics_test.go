package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// brokerSubsForMetrics constructs a Subsystems wired with the given deps and a
// fixed seeder available for the requested model. Returns the assembled
// Subsystems and the underlying registry so callers can drive offer responses.
func brokerSubsForMetrics(t *testing.T, deps Deps, seederID ids.IdentityID, model string) (*Subsystems, *fakeRegistry) {
	t.Helper()
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, seederID, 0.9, model))
	deps.Registry = fr
	subs, err := Open(defaultBrokerCfg(), testSettlementCfg(), deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = subs.Close() })
	return subs, fr
}

// TestSubsystems_Collector_RegistersWithRegistry verifies that the composite
// collector returned by Subsystems.Collector() is registrable against a fresh
// registry without conflicting descriptors.
func TestSubsystems_Collector_RegistersWithRegistry(t *testing.T) {
	subs, err := Open(defaultBrokerCfg(), testSettlementCfg(), testDeps(t))
	require.NoError(t, err)
	defer subs.Close()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(subs.Collector()))
}

// TestSubsystems_Submit_DecisionsByResult drives Submit through three return
// paths and asserts the SubmitDecisions counter accumulates the right label
// values: admit, frozen, no_capacity. Mirrors the spec test plan
// (one Assignment + one Queued + one Rejected, scrape, assert).
func TestSubsystems_Submit_DecisionsByResult(t *testing.T) {
	const model = "claude-sonnet-4-6"

	// --- admit path
	depsAdmit := testDeps(t)
	p := withFakePusher(t, &depsAdmit)
	queueDecision(p, true, bytesAllB(32, 0xAA))
	subsAdmit, _ := brokerSubsForMetrics(t, depsAdmit, ids.IdentityID{1}, model)

	env := makeAdmittedEnvelope(t, model, 100, 200, 1_000_000)
	res, err := subsAdmit.Broker.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)

	require.InDelta(t, 1.0, testutil.ToFloat64(subsAdmit.Broker.metrics.SubmitDecisions.WithLabelValues("admit")), 0)
	require.InDelta(t, 1.0, testutil.ToFloat64(subsAdmit.Broker.metrics.OfferAttempts.WithLabelValues("accept")), 0)

	// --- frozen path
	depsFrozen := testDeps(t)
	withFakePusher(t, &depsFrozen)
	rep := newStubReputation()
	depsFrozen.Reputation = rep
	subsFrozen, _ := brokerSubsForMetrics(t, depsFrozen, ids.IdentityID{2}, model)

	envFrozen := makeAdmittedEnvelope(t, model, 100, 200, 1_000_000)
	var consumer ids.IdentityID
	copy(consumer[:], envFrozen.Body.ConsumerId)
	rep.frozen[consumer] = true
	_, err = subsFrozen.Broker.Submit(context.Background(), envFrozen)
	require.ErrorIs(t, err, ErrIdentityFrozen)

	require.InDelta(t, 1.0, testutil.ToFloat64(subsFrozen.Broker.metrics.SubmitDecisions.WithLabelValues("frozen")), 0)

	// --- no_capacity path
	depsNoCap := testDeps(t)
	depsNoCap.Registry = newFakeRegistry() // empty registry → no eligible seeder
	withFakePusher(t, &depsNoCap)
	subsNoCap, err := Open(defaultBrokerCfg(), testSettlementCfg(), depsNoCap)
	require.NoError(t, err)
	defer subsNoCap.Close()

	envNoCap := makeAdmittedEnvelope(t, model, 100, 200, 1_000_000)
	resNoCap, err := subsNoCap.Broker.Submit(context.Background(), envNoCap)
	require.NoError(t, err)
	require.Equal(t, OutcomeNoCapacity, resNoCap.Outcome)

	require.InDelta(t, 1.0, testutil.ToFloat64(subsNoCap.Broker.metrics.SubmitDecisions.WithLabelValues("no_capacity")), 0)
}

// TestSubsystems_QueueDrain_BumpsPopsAndOutcomes drives one RegisterQueued +
// TriggerQueueDrain cycle and asserts QueueDrainPops + QueueDrainAdmitOutcomes
// reflect the outcome.
func TestSubsystems_QueueDrain_BumpsPopsAndOutcomes(t *testing.T) {
	const model = "claude-sonnet-4-6"

	deps := testDeps(t)
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	requestID := [16]byte{0xEE}
	deps.Admission = &fakeAdmissionWithEntries{
		entries: []admission.QueueEntry{{RequestID: requestID, CreditScore: 0.9, EnqueuedAt: time.Now()}},
	}

	subs, _ := brokerSubsForMetrics(t, deps, ids.IdentityID{1}, model)

	env := makeAdmittedEnvelope(t, model, 1, 1, 1_000_000)
	delivered := make(chan *Result, 1)
	subs.Broker.RegisterQueued(env, requestID, func(r *Result) { delivered <- r })
	subs.Broker.TriggerQueueDrain()

	select {
	case r := <-delivered:
		require.Equal(t, OutcomeAdmit, r.Outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("queue drain did not deliver")
	}

	require.InDelta(t, 1.0, testutil.ToFloat64(subs.Broker.metrics.QueueDrainPops), 0)
	require.InDelta(t, 1.0, testutil.ToFloat64(subs.Broker.metrics.QueueDrainAdmitOutcomes.WithLabelValues("admit")), 0)
}

// TestSubsystems_Reaper_BumpsTTLExpired drives the reservation TTL reaper
// against an expired slot and asserts the counter increments by the count of
// swept entries.
func TestSubsystems_Reaper_BumpsTTLExpired(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0xF1}
	mgr.Inflight.Insert(&session.Request{
		RequestID: requestID,
		State:     session.StateSelecting,
	})
	require.NoError(t, mgr.Reservations.Reserve(requestID, ids.IdentityID{0xAA}, 50, 1000, time.Now().Add(-time.Second)))

	m := newBrokerMetrics()
	s, err := openSettlementWithMetrics(testSettlementCfg(), deps, mgr, m)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	require.InDelta(t, 1.0, testutil.ToFloat64(m.ReservationTTLExpired), 0)
}

// errLedger is a ledger stub that fails every AppendUsage so settlement records
// a ledger-append failure.
type errLedger struct{}

func (errLedger) Tip(_ context.Context) (uint64, []byte, bool, error) {
	return 0, make([]byte, 32), false, nil
}

var errFakeAppend = errors.New("fake append failure")

func (errLedger) AppendUsage(_ context.Context, _ ledger.UsageRecord) (*tbproto.Entry, error) {
	return nil, errFakeAppend
}

// TestSubsystems_Settlement_BumpsLedgerAppendFailure drives appendUsageEntry
// through a failing ledger and asserts LedgerAppendFailure increments.
func TestSubsystems_Settlement_BumpsLedgerAppendFailure(t *testing.T) {
	deps := testDeps(t)
	deps.Ledger = errLedger{}
	mgr := session.New()

	requestID := [16]byte{0x77}
	consumer := ids.IdentityID{0xCC}
	seeder := ids.IdentityID{0xDD}
	seederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	req := &session.Request{
		RequestID:      requestID,
		ConsumerID:     consumer,
		AssignedSeeder: seeder,
		SeederPubkey:   seederPub,
		State:          session.StateServing,
	}
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumer, 10, 1000, time.Now().Add(time.Hour))

	m := newBrokerMetrics()
	cfg := testSettlementCfg()
	cfg.StaleTipRetries = 0
	s, err := openSettlementWithMetrics(cfg, deps, mgr, m)
	require.NoError(t, err)
	defer s.Close()

	rec := ledger.UsageRecord{
		PrevHash:           make([]byte, 32),
		Seq:                1,
		ConsumerID:         consumer[:],
		SeederID:           seeder[:],
		Model:              "claude-sonnet-4-6",
		Timestamp:          uint64(time.Now().Unix()), //nolint:gosec
		RequestID:          requestID[:],
		ConsumerSigMissing: true,
	}
	s.appendUsageEntry(context.Background(), req, rec)

	require.InDelta(t, 1.0, testutil.ToFloat64(m.LedgerAppendFailure), 0)
}

// TestSubsystems_PendingQueueDepth_DynamicGauge verifies the queue-depth gauge
// is computed at scrape time from b.pendingQueued, mirroring the
// admission/metrics.go dynamic-collector pattern.
func TestSubsystems_PendingQueueDepth_DynamicGauge(t *testing.T) {
	deps := testDeps(t)
	withFakePusher(t, &deps)
	deps.Admission = &fakeAdmissionWithEntries{} // never pops anything
	subs, _ := brokerSubsForMetrics(t, deps, ids.IdentityID{1}, "claude-sonnet-4-6")

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(subs.Collector()))

	// Initial scrape: depth 0.
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP broker_pending_queue_depth Number of RegisterQueued entries currently waiting for drain.
# TYPE broker_pending_queue_depth gauge
broker_pending_queue_depth 0
`), "broker_pending_queue_depth"))

	// Register two queued requests; depth should be 2.
	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 1, 1, 1_000_000)
	subs.Broker.RegisterQueued(env, [16]byte{0xA1}, func(*Result) {})
	subs.Broker.RegisterQueued(env, [16]byte{0xA2}, func(*Result) {})

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP broker_pending_queue_depth Number of RegisterQueued entries currently waiting for drain.
# TYPE broker_pending_queue_depth gauge
broker_pending_queue_depth 2
`), "broker_pending_queue_depth"))

	// Cancel one; depth should drop to 1.
	subs.Broker.CancelQueued([16]byte{0xA1})
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP broker_pending_queue_depth Number of RegisterQueued entries currently waiting for drain.
# TYPE broker_pending_queue_depth gauge
broker_pending_queue_depth 1
`), "broker_pending_queue_depth"))
}

// lockedFakeRegistry wraps fakeRegistry with a mutex so concurrent submits
// don't race on the underlying slice. The race we care about catching is in
// the production metrics path, not in the test fixture.
type lockedFakeRegistry struct {
	mu sync.Mutex
	fr *fakeRegistry
}

func newLockedFakeRegistry() *lockedFakeRegistry {
	return &lockedFakeRegistry{fr: newFakeRegistry()}
}

func (l *lockedFakeRegistry) Add(rec registry.SeederRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fr.Add(rec)
}

func (l *lockedFakeRegistry) Match(f registry.Filter) []registry.SeederRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.Match(f)
}

func (l *lockedFakeRegistry) Get(id ids.IdentityID) (registry.SeederRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.Get(id)
}

func (l *lockedFakeRegistry) IncLoad(id ids.IdentityID) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.IncLoad(id)
}

func (l *lockedFakeRegistry) DecLoad(id ids.IdentityID) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.DecLoad(id)
}

// TestSubsystems_Submit_RaceClean runs Submit concurrently against a metrics-
// instrumented Broker to catch counter-bumping races.
func TestSubsystems_Submit_RaceClean(t *testing.T) {
	deps := testDeps(t)
	fr := newLockedFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	// Pusher that always accepts, with a buffered channel large enough for
	// every concurrent call.
	const N = 8
	offerCh := make(chan *tbproto.OfferDecision, N*2)
	for range N * 2 {
		offerCh <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: bytesAllB(32, 0xAB)}
	}
	deps.Pusher = &fakePusher{offerCh: offerCh, ok: true}

	cfg := defaultBrokerCfg()
	// Don't run out of attempts under concurrent IncLoad pressure.
	cfg.LoadThreshold = 0 // unlimited
	subs, err := Open(cfg, testSettlementCfg(), deps)
	require.NoError(t, err)
	defer subs.Close()

	var wg sync.WaitGroup
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 1, 1, 1_000_000)
			// Use distinct consumer IDs so reservations don't collide.
			env.Body.ConsumerId = bytesAllB(32, byte(i+1))
			_, _ = subs.Broker.Submit(context.Background(), env)
		}(i)
	}
	wg.Wait()

	got := testutil.ToFloat64(subs.Broker.metrics.SubmitDecisions.WithLabelValues("admit"))
	require.InDelta(t, float64(N), got, 0)
}
