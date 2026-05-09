package broker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

func TestReaper_ExpiresStaleReservation(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	seeder := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seeder)
	_, _ = fr.IncLoad(seeder.IdentityID) // simulate broker's IncLoad on accept
	deps.Registry = fr

	mgr := session.New()

	requestID := [16]byte{0xEE}
	mgr.Inflight.Insert(&session.Request{
		RequestID:      requestID,
		AssignedSeeder: ids.IdentityID{0xDD},
		State:          session.StateAssigned,
	})
	require.NoError(t, mgr.Reservations.Reserve(requestID, ids.IdentityID{0xCC}, 100, 1000, time.Now().Add(-time.Second)))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	require.Equal(t, uint64(0), mgr.Reservations.Reserved(ids.IdentityID{0xCC}))
	req, _ := mgr.Inflight.Get(requestID)
	require.Equal(t, session.StateFailed, req.State)
}

func TestReaper_SelectingStateTransitioned(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0xF1}
	mgr.Inflight.Insert(&session.Request{
		RequestID: requestID,
		State:     session.StateSelecting,
	})
	require.NoError(t, mgr.Reservations.Reserve(requestID, ids.IdentityID{0xAA}, 50, 1000, time.Now().Add(-time.Second)))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	req, _ := mgr.Inflight.Get(requestID)
	require.Equal(t, session.StateFailed, req.State)
}

func TestReaper_NoExpiryIfNotExpired(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0xF2}
	mgr.Inflight.Insert(&session.Request{
		RequestID: requestID,
		State:     session.StateAssigned,
	})
	// Reservation expires in the future — should NOT be swept.
	require.NoError(t, mgr.Reservations.Reserve(requestID, ids.IdentityID{0xBB}, 50, 1000, time.Now().Add(time.Hour)))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	req, _ := mgr.Inflight.Get(requestID)
	require.Equal(t, session.StateAssigned, req.State) // not yet failed
	require.Equal(t, uint64(50), mgr.Reservations.Reserved(ids.IdentityID{0xBB}))
}
