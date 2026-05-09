package admin

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

func TestAuth_RejectsMissingHeader(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/health", "", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("WWW-Authenticate"))
}

func TestAuth_RejectsBadToken(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/health", "wrong-token", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealth_OK(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/health", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "ok", out["status"])
	assert.Equal(t, "test", out["version"])
	assert.NotEmpty(t, out["time"])
}

func TestStats_NoTip(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/stats", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.EqualValues(t, 7, out["connections"])
	assert.EqualValues(t, 60, out["merkle_root_minutes"])
	assert.Nil(t, out["broker_reqs_per_sec"])
	ledger, ok := out["ledger"].(map[string]any)
	require.True(t, ok)
	assert.Nil(t, ledger["tip_seq"])
	assert.Nil(t, ledger["tip_hash"])
}

func TestStats_WithTip(t *testing.T) {
	tipHash := []byte{0xde, 0xad, 0xbe, 0xef}
	srv := newTestServer(t, func(d *Deps) {
		d.Ledger = &stubLedger{tipSeq: 42, tipHash: tipHash, hasTip: true}
	})
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/stats", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	ledger := out["ledger"].(map[string]any)
	assert.EqualValues(t, 42, ledger["tip_seq"])
	assert.Equal(t, hex.EncodeToString(tipHash), ledger["tip_hash"])
}

func TestStats_LedgerError(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) {
		d.Ledger = &stubLedger{tipErr: errors.New("disk gone")}
	})
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/stats", "test-token", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPeers_EmptyFederation(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/peers", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "not_implemented", out["federation_state"])
	assert.EqualValues(t, 7, out["connected_quic"])
}

func TestPeersAdd_Unimplemented(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", "{}")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestPeersRemove_Unimplemented(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", "{}")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestIdentity_BadHex(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/identity/notHex", "test-token", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestIdentity_NotFound(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	id := "00" + strings.Repeat("ff", 31)
	resp := adminReq(t, "GET", srv.URL+"/identity/"+id, "test-token", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIdentity_FromRegistry(t *testing.T) {
	var id ids.IdentityID
	for i := range id {
		id[i] = byte(i)
	}
	rec := registry.SeederRecord{
		IdentityID:       id,
		Available:        true,
		HeadroomEstimate: 0.42,
		ReputationScore:  0.85,
		Load:             3,
		LastHeartbeat:    time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		Capabilities:     registry.Capabilities{Models: []string{"claude-opus-4-7"}},
	}
	srv := newTestServer(t, func(d *Deps) {
		d.Registry = &stubRegistry{by: map[ids.IdentityID]registry.SeederRecord{id: rec}}
	})
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/identity/"+hex.EncodeToString(id[:]), "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	seeder := out["seeder"].(map[string]any)
	assert.Equal(t, true, seeder["available"])
	assert.InDelta(t, 0.42, seeder["headroom_estimate"], 1e-9)
	assert.InDelta(t, 0.85, seeder["reputation_score"], 1e-9)
	assert.EqualValues(t, 3, seeder["load"])
}

func TestIdentity_FromLedgerOnly(t *testing.T) {
	var id ids.IdentityID
	for i := range id {
		id[i] = 0xaa
	}
	bal := &tbproto.SignedBalanceSnapshot{
		Body: &tbproto.BalanceSnapshotBody{
			IdentityId:  id[:],
			Credits:     9000,
			ChainTipSeq: 17,
			IssuedAt:    1700000000,
			ExpiresAt:   1700000600,
		},
	}
	srv := newTestServer(t, func(d *Deps) {
		key := [32]byte(id)
		d.Ledger = &stubLedger{balanceBy: map[[32]byte]*tbproto.SignedBalanceSnapshot{key: bal}}
	})
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/identity/"+hex.EncodeToString(id[:]), "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	balance := out["balance"].(map[string]any)
	assert.EqualValues(t, 9000, balance["credits"])
	assert.EqualValues(t, 17, balance["chain_tip_seq"])
}

func TestMaintenance_TriggersCallbackAndReturns202(t *testing.T) {
	var fired atomic.Int32
	done := make(chan struct{}, 1)
	srv := newTestServer(t, func(d *Deps) {
		d.TriggerMaintenance = func() {
			fired.Add(1)
			done <- struct{}{}
		}
	})
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/maintenance", "test-token", "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("trigger callback not invoked")
	}
	assert.EqualValues(t, 1, fired.Load())
}
