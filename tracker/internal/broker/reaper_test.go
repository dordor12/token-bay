package broker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestReaper_ExpiresStaleReservation(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	seeder := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seeder)
	_, _ = fr.IncLoad(seeder.IdentityID) // simulate broker's IncLoad on accept
	deps.Registry = fr

	inflt := NewInflight()
	resv := NewReservations()

	requestID := [16]byte{0xEE}
	inflt.Insert(&Request{
		RequestID:      requestID,
		AssignedSeeder: ids.IdentityID{0xDD},
		State:          StateAssigned,
	})
	require.NoError(t, resv.Reserve(requestID, ids.IdentityID{0xCC}, 100, 1000, time.Now().Add(-time.Second)))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, resv)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	require.Equal(t, uint64(0), resv.Reserved(ids.IdentityID{0xCC}))
	req, _ := inflt.Get(requestID)
	require.Equal(t, StateFailed, req.State)
}

func TestReaper_SelectingStateTransitioned(t *testing.T) {
	deps := testDeps(t)
	inflt := NewInflight()
	resv := NewReservations()

	requestID := [16]byte{0xF1}
	inflt.Insert(&Request{
		RequestID: requestID,
		State:     StateSelecting,
	})
	require.NoError(t, resv.Reserve(requestID, ids.IdentityID{0xAA}, 50, 1000, time.Now().Add(-time.Second)))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, resv)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	req, _ := inflt.Get(requestID)
	require.Equal(t, StateFailed, req.State)
}

func TestReaper_NoExpiryIfNotExpired(t *testing.T) {
	deps := testDeps(t)
	inflt := NewInflight()
	resv := NewReservations()

	requestID := [16]byte{0xF2}
	inflt.Insert(&Request{
		RequestID: requestID,
		State:     StateAssigned,
	})
	// Reservation expires in the future — should NOT be swept.
	require.NoError(t, resv.Reserve(requestID, ids.IdentityID{0xBB}, 50, 1000, time.Now().Add(time.Hour)))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, resv)
	require.NoError(t, err)
	defer s.Close()

	s.runReap(time.Now())

	req, _ := inflt.Get(requestID)
	require.Equal(t, StateAssigned, req.State) // not yet failed
	require.Equal(t, uint64(50), resv.Reserved(ids.IdentityID{0xBB}))
}
