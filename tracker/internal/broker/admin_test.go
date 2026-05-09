package broker

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// openTestSubsystems returns a *Subsystems ready for admin tests.
func openTestSubsystems(t *testing.T) *Subsystems {
	t.Helper()
	sub, err := Open(defaultBrokerCfg(), testSettlementCfg(), testDeps(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// insertInflight is a test helper that adds a Request directly to the shared
// session.Manager's Inflight store.
func insertInflight(sub *Subsystems, req *session.Request) {
	sub.Broker.mgr.Inflight.Insert(req)
}

// insertReservation is a test helper that adds a reservation to the shared
// session.Manager's Reservations store. snapshotCredits is set high enough
// to always admit the reservation.
func insertReservation(sub *Subsystems, reqID [16]byte, consumer ids.IdentityID, amount uint64) {
	_ = sub.Broker.mgr.Reservations.Reserve(reqID, consumer, amount, 1_000_000_000, time.Now().Add(time.Hour))
}

// doRequest performs an HTTP request against the AdminHandler.
func doRequest(t *testing.T, sub *Subsystems, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	sub.AdminHandler().ServeHTTP(rec, req)
	return rec
}

// decodeJSON decodes a JSON body from recorder into dst.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(rec.Body).Decode(dst))
}

// ---------------------------------------------------------------------------

func TestAdmin_ListInflight(t *testing.T) {
	sub := openTestSubsystems(t)

	consumerA := ids.IdentityID{0xAA}
	consumerB := ids.IdentityID{0xBB}
	reqA := [16]byte{0x01}
	reqB := [16]byte{0x02}

	insertInflight(sub, &session.Request{
		RequestID:  reqA,
		ConsumerID: consumerA,
		State:      session.StateSelecting,
		StartedAt:  time.Now().Add(-5 * time.Second),
	})
	insertInflight(sub, &session.Request{
		RequestID:  reqB,
		ConsumerID: consumerB,
		State:      session.StateAssigned,
		StartedAt:  time.Now().Add(-10 * time.Second),
	})

	rec := doRequest(t, sub, http.MethodGet, "/broker/inflight")
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	decodeJSON(t, rec, &list)
	require.Len(t, list, 2)

	// Verify expected fields exist in each entry.
	for _, entry := range list {
		require.Contains(t, entry, "request_id")
		require.Contains(t, entry, "consumer_id")
		require.Contains(t, entry, "state")
		require.Contains(t, entry, "age_seconds")
	}

	// Collect request_ids from the response.
	gotIDs := make(map[string]bool)
	for _, e := range list {
		gotIDs[e["request_id"].(string)] = true
	}
	require.True(t, gotIDs[hex.EncodeToString(reqA[:])])
	require.True(t, gotIDs[hex.EncodeToString(reqB[:])])
}

func TestAdmin_GetInflight_NotFound(t *testing.T) {
	sub := openTestSubsystems(t)
	// Random request_id that was never inserted.
	randomID := hex.EncodeToString(make([]byte, 16)) // all-zero hex
	rec := doRequest(t, sub, http.MethodGet, "/broker/inflight/"+randomID)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdmin_GetInflight_Detail(t *testing.T) {
	sub := openTestSubsystems(t)

	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	reqID := [16]byte{0x03}

	insertInflight(sub, &session.Request{
		RequestID:      reqID,
		ConsumerID:     consumerID,
		AssignedSeeder: seederID,
		State:          session.StateAssigned,
		StartedAt:      time.Now(),
	})

	rec := doRequest(t, sub, http.MethodGet, "/broker/inflight/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusOK, rec.Code)

	var detail map[string]any
	decodeJSON(t, rec, &detail)
	require.Equal(t, hex.EncodeToString(reqID[:]), detail["request_id"])
	require.Equal(t, hex.EncodeToString(consumerID[:]), detail["consumer_id"])
	require.Equal(t, "assigned", detail["state"])
	require.Equal(t, hex.EncodeToString(seederID[:]), detail["seeder_id"])
}

func TestAdmin_ListReservations(t *testing.T) {
	sub := openTestSubsystems(t)

	consumerA := ids.IdentityID{0xAA}
	reqA := [16]byte{0x11}
	reqB := [16]byte{0x12}

	insertReservation(sub, reqA, consumerA, 1000)
	insertReservation(sub, reqB, consumerA, 500)

	rec := doRequest(t, sub, http.MethodGet, "/broker/reservations")
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	decodeJSON(t, rec, &list)
	require.Len(t, list, 1) // one consumer

	entry := list[0]
	require.Contains(t, entry, "consumer_id")
	require.Contains(t, entry, "total")
	require.Contains(t, entry, "slots")
	slots := entry["slots"].([]any)
	require.Len(t, slots, 2)
}

func TestAdmin_ForceReleaseReservation(t *testing.T) {
	sub := openTestSubsystems(t)

	consumer := ids.IdentityID{0xAA}
	reqID := [16]byte{0x21}
	insertReservation(sub, reqID, consumer, 1000)

	// Confirm reservation exists.
	require.Equal(t, uint64(1000), sub.Broker.mgr.Reservations.Reserved(consumer))

	rec := doRequest(t, sub, http.MethodPost,
		"/broker/reservations/release/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	require.Equal(t, true, resp["released"])

	// Reservation should be gone.
	require.Equal(t, uint64(0), sub.Broker.mgr.Reservations.Reserved(consumer))
}

func TestAdmin_ForceReleaseReservation_NotFound(t *testing.T) {
	sub := openTestSubsystems(t)
	// Non-existent reservation ID.
	reqID := [16]byte{0xFF}
	rec := doRequest(t, sub, http.MethodPost,
		"/broker/reservations/release/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdmin_ForceFailInflight(t *testing.T) {
	sub := openTestSubsystems(t)

	consumer := ids.IdentityID{0xCC}
	reqID := [16]byte{0x31}
	insertInflight(sub, &session.Request{
		RequestID:  reqID,
		ConsumerID: consumer,
		State:      session.StateAssigned,
		StartedAt:  time.Now(),
	})

	rec := doRequest(t, sub, http.MethodPost,
		"/broker/inflight/fail/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	require.Equal(t, true, resp["failed"])

	// State should now be failed.
	req, ok := sub.Broker.mgr.Inflight.Get(reqID)
	require.True(t, ok)
	require.Equal(t, session.StateFailed, req.State)
}

func TestAdmin_ForceFailInflight_NotFound(t *testing.T) {
	sub := openTestSubsystems(t)
	reqID := [16]byte{0xFF}
	rec := doRequest(t, sub, http.MethodPost,
		"/broker/inflight/fail/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
