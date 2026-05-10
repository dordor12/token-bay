package reputation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func newTestStorage(t *testing.T) *storage {
	t.Helper()
	s, err := openStorage(context.Background(),
		filepath.Join(t.TempDir(), "rep.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorageState_EnsureAndRead(t *testing.T) {
	s := newTestStorage(t)
	var id ids.IdentityID
	id[0] = 0xAA
	now := time.Unix(1714000000, 0)

	require.NoError(t, s.ensureState(context.Background(), id, now))

	row, ok, err := s.readState(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, StateOK, row.State)
	require.Equal(t, now.Unix(), row.Since.Unix())
	require.Equal(t, now.Unix(), row.FirstSeenAt.Unix())
	require.Empty(t, row.Reasons)
}

func TestStorageState_EnsureIsIdempotent(t *testing.T) {
	s := newTestStorage(t)
	var id ids.IdentityID
	id[0] = 0xBB
	first := time.Unix(1, 0)
	later := time.Unix(2, 0)

	require.NoError(t, s.ensureState(context.Background(), id, first))
	require.NoError(t, s.ensureState(context.Background(), id, later))

	row, ok, err := s.readState(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.Unix(), row.FirstSeenAt.Unix(),
		"first_seen_at must not be updated by a second ensureState")
}

func TestStorageState_TransitionAppendsReason(t *testing.T) {
	s := newTestStorage(t)
	var id ids.IdentityID
	id[0] = 0xCC
	t0 := time.Unix(100, 0)
	require.NoError(t, s.ensureState(context.Background(), id, t0))

	r := ReasonRecord{
		Kind: "zscore", Signal: "broker_request", Z: -5.2,
		Window: "24h", At: t0.Unix(),
	}
	require.NoError(t, s.transition(context.Background(), id,
		StateAudit, r, t0))

	row, _, err := s.readState(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, StateAudit, row.State)
	require.Len(t, row.Reasons, 1)
	require.Equal(t, r, row.Reasons[0])
}

func TestStorageState_TransitionRefusesFromFrozen(t *testing.T) {
	s := newTestStorage(t)
	var id ids.IdentityID
	id[0] = 0xDD
	now := time.Unix(1, 0)
	require.NoError(t, s.ensureState(context.Background(), id, now))
	r := ReasonRecord{
		Kind: "breach", BreachKind: "invalid_proof_signature",
		At: now.Unix(),
	}
	require.NoError(t, s.transition(context.Background(), id,
		StateFrozen, r, now))

	err := s.transition(context.Background(), id, StateOK,
		ReasonRecord{Kind: "manual", At: now.Unix()}, now)
	require.ErrorIs(t, err, errInvalidTransition)
}

func TestStorageState_ReasonsRoundTripJSON(t *testing.T) {
	s := newTestStorage(t)
	var id ids.IdentityID
	id[0] = 0xEE
	now := time.Unix(1, 0)
	require.NoError(t, s.ensureState(context.Background(), id, now))

	r := ReasonRecord{
		Kind: "zscore", Signal: "exhaustion_claim_rate",
		Z: 6.1, Window: "1h", At: now.Unix(),
	}
	require.NoError(t, s.transition(context.Background(), id,
		StateAudit, r, now))

	var raw string
	require.NoError(t, s.db.QueryRow(
		`SELECT reasons FROM rep_state WHERE identity_id = ?`,
		id[:]).Scan(&raw))

	var got []ReasonRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Len(t, got, 1)
	require.Equal(t, r, got[0])
}
