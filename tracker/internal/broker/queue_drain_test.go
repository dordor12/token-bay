package broker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/admission"
)

// fakeAdmissionWithEntries lets tests preload PopReadyForBroker with queued entries.
type fakeAdmissionWithEntries struct {
	entries  []admission.QueueEntry
	pressure float64
}

func (a *fakeAdmissionWithEntries) PopReadyForBroker(now time.Time, minServePriority float64) (admission.QueueEntry, bool) {
	if len(a.entries) == 0 {
		return admission.QueueEntry{}, false
	}
	e := a.entries[0]
	a.entries = a.entries[1:]
	return e, true
}
func (a *fakeAdmissionWithEntries) PressureGauge() float64 { return a.pressure }
func (a *fakeAdmissionWithEntries) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return admission.Result{Outcome: admission.OutcomeAdmit}
}

func TestRegisterQueued_DeliversOnDrain(t *testing.T) {
	const model = "claude-sonnet-4-6"

	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, model))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	requestID := [16]byte{0xEE}
	deps.Admission = &fakeAdmissionWithEntries{
		entries:  []admission.QueueEntry{{RequestID: requestID, CreditScore: 0.9, EnqueuedAt: time.Now()}},
		pressure: 0,
	}

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, model, 1, 1, 1_000_000)
	delivered := make(chan *Result, 1)
	b.RegisterQueued(env, requestID, func(r *Result) { delivered <- r })

	b.TriggerQueueDrain()

	select {
	case r := <-delivered:
		require.Equal(t, OutcomeAdmit, r.Outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("queue drain did not deliver")
	}
}

func TestQueueDrain_StopsOnClose(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry()
	withFakePusher(t, &deps)
	deps.Admission = &fakeAdmissionWithEntries{}

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	require.NoError(t, b.Close())
}

// TestQueueDrain_StaleEntry_NoPanic verifies the goroutine doesn't panic when
// admission pops an entry that has no matching pendingQueued cache (stale
// entry).
func TestQueueDrain_StaleEntry_NoPanic(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry()
	withFakePusher(t, &deps)
	deps.Admission = &fakeAdmissionWithEntries{
		entries: []admission.QueueEntry{{RequestID: [16]byte{0x99}, CreditScore: 0.9, EnqueuedAt: time.Now()}},
	}
	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()
	b.drainOnce(context.Background()) // should not panic
}
