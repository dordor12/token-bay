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
	"github.com/token-bay/token-bay/tracker/internal/registry"
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

// TestAdmin_ForceFailInflight_ReclaimsSeederLoad pins the operator-recovery
// invariant: force-failing an ASSIGNED request must return the seeder's
// registry load slot — exactly what settlement completion and the TTL reaper
// do when a request leaves the Assigned state. Without this, every operator
// intervention on a stuck request silently and permanently shrinks that
// seeder's effective capacity (the selector filters it at LoadThreshold) until
// it re-advertises or the process restarts. The credit reservation is a
// separate primitive (POST /broker/reservations/release) and is intentionally
// left untouched here.
func TestAdmin_ForceFailInflight_ReclaimsSeederLoad(t *testing.T) {
	sub := openTestSubsystems(t)

	fr := sub.Broker.deps.Registry.(*fakeRegistry)
	consumer := ids.IdentityID{0xC0}
	seeder := ids.IdentityID{0x5E}
	reqID := [16]byte{0x41}

	// A seeder carrying one in-flight assignment: load=1, plus the request's
	// held credit reservation and its Assigned in-flight record.
	fr.Add(registry.SeederRecord{IdentityID: seeder, Available: true})
	_, err := fr.IncLoad(seeder)
	require.NoError(t, err)
	insertReservation(sub, reqID, consumer, 1000)
	insertInflight(sub, &session.Request{
		RequestID:      reqID,
		ConsumerID:     consumer,
		AssignedSeeder: seeder,
		State:          session.StateAssigned,
		StartedAt:      time.Now(),
	})
	require.Equal(t, 1, mustGet(t, fr, seeder).Load, "precondition: load held")
	require.Equal(t, uint64(1000), sub.Broker.mgr.Reservations.Reserved(consumer))

	rec := doRequest(t, sub, http.MethodPost,
		"/broker/inflight/fail/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusOK, rec.Code)

	req, ok := sub.Broker.mgr.Inflight.Get(reqID)
	require.True(t, ok)
	require.Equal(t, session.StateFailed, req.State)
	require.Equal(t, 0, mustGet(t, fr, seeder).Load, "seeder load slot reclaimed on operator force-fail")
	require.Equal(t, uint64(1000), sub.Broker.mgr.Reservations.Reserved(consumer),
		"credit reservation is left for the separate reservations/release operator step")
}

func mustGet(t *testing.T, fr *fakeRegistry, id ids.IdentityID) registry.SeederRecord {
	t.Helper()
	rec, ok := fr.Get(id)
	require.True(t, ok)
	return rec
}

func TestAdmin_ForceFailInflight_NotFound(t *testing.T) {
	sub := openTestSubsystems(t)
	reqID := [16]byte{0xFF}
	rec := doRequest(t, sub, http.MethodPost,
		"/broker/inflight/fail/"+hex.EncodeToString(reqID[:]))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
