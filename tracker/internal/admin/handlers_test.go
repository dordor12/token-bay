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

func TestPeers_NoFederation(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/peers", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "disabled", out["federation_state"])
	assert.EqualValues(t, 7, out["connected_quic"])
	peers, ok := out["peers"].([]any)
	require.True(t, ok)
	assert.Empty(t, peers)
}

func TestPeers_WithFederation(t *testing.T) {
	var tid ids.TrackerID
	for i := range tid {
		tid[i] = byte(i)
	}
	pk := make([]byte, 32)
	for i := range pk {
		pk[i] = 0x42
	}
	stub := &stubFederation{
		listen: "127.0.0.1:9443",
		peers: []PeerSummary{{
			TrackerID:   tid,
			PubKey:      pk,
			Addr:        "peer.example:9443",
			Region:      "eu-1",
			State:       "steady",
			Since:       time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
			HealthScore: 0.87,
		}},
	}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/peers", "test-token", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "enabled", out["federation_state"])
	assert.Equal(t, "127.0.0.1:9443", out["listen_addr"])

	peers, ok := out["peers"].([]any)
	require.True(t, ok)
	require.Len(t, peers, 1)
	row := peers[0].(map[string]any)
	assert.Equal(t, hex.EncodeToString(tid[:]), row["tracker_id"])
	assert.Equal(t, hex.EncodeToString(pk), row["pubkey"])
	assert.Equal(t, "peer.example:9443", row["addr"])
	assert.Equal(t, "eu-1", row["region"])
	assert.Equal(t, "steady", row["state"])
	assert.InDelta(t, 0.87, row["health_score"], 1e-9)
	assert.Equal(t, "2026-05-10T10:00:00Z", row["since"])
}

func TestPeersAdd_NoFederation_501(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", "{}")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestPeersAdd_BadJSON_400(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) { d.Federation = &stubFederation{} })
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", "not json")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPeersAdd_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no tracker_id", `{"pubkey":"` + strings.Repeat("aa", 32) + `","addr":"x:1"}`},
		{"bad tracker_id hex", `{"tracker_id":"zz","pubkey":"` + strings.Repeat("aa", 32) + `","addr":"x:1"}`},
		{"short tracker_id", `{"tracker_id":"aabbcc","pubkey":"` + strings.Repeat("aa", 32) + `","addr":"x:1"}`},
		{"bad pubkey hex", `{"tracker_id":"` + strings.Repeat("ab", 32) + `","pubkey":"zz","addr":"x:1"}`},
		{"short pubkey", `{"tracker_id":"` + strings.Repeat("ab", 32) + `","pubkey":"aabb","addr":"x:1"}`},
		{"no addr", `{"tracker_id":"` + strings.Repeat("ab", 32) + `","pubkey":"` + strings.Repeat("aa", 32) + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t, func(d *Deps) { d.Federation = &stubFederation{} })
			defer srv.Close()

			resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", c.body)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestPeersAdd_Success_202(t *testing.T) {
	stub := &stubFederation{}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	tidHex := strings.Repeat("ab", 32)
	pkHex := strings.Repeat("cd", 32)
	body := `{"tracker_id":"` + tidHex + `","pubkey":"` + pkHex + `","addr":"peer.example:9443","region":"eu-1"}`

	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	require.Len(t, stub.added, 1)
	got := stub.added[0]
	assert.Equal(t, hex.EncodeToString(got.TrackerID[:]), tidHex)
	assert.Equal(t, hex.EncodeToString(got.PubKey), pkHex)
	assert.Equal(t, "peer.example:9443", got.Addr)
	assert.Equal(t, "eu-1", got.Region)
}

func TestPeersAdd_Conflict_409(t *testing.T) {
	stub := &stubFederation{addErr: ErrPeerExists}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	body := `{"tracker_id":"` + strings.Repeat("ab", 32) + `","pubkey":"` + strings.Repeat("cd", 32) + `","addr":"x:1"}`
	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", body)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestPeersAdd_OtherError_500(t *testing.T) {
	stub := &stubFederation{addErr: errors.New("boom")}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	body := `{"tracker_id":"` + strings.Repeat("ab", 32) + `","pubkey":"` + strings.Repeat("cd", 32) + `","addr":"x:1"}`
	resp := adminReq(t, "POST", srv.URL+"/peers/add", "test-token", body)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPeersRemove_NoFederation_501(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", "{}")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

func TestPeersRemove_BadJSON_400(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) { d.Federation = &stubFederation{} })
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", "not json")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPeersRemove_BadTrackerID_400(t *testing.T) {
	srv := newTestServer(t, func(d *Deps) { d.Federation = &stubFederation{} })
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", `{"tracker_id":"zz"}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPeersRemove_Unknown_404(t *testing.T) {
	stub := &stubFederation{depeerErr: ErrPeerUnknown}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	body := `{"tracker_id":"` + strings.Repeat("ab", 32) + `"}`
	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", body)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPeersRemove_Success_200(t *testing.T) {
	stub := &stubFederation{}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	tidHex := strings.Repeat("ab", 32)
	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", `{"tracker_id":"`+tidHex+`"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Len(t, stub.depeered, 1)
	assert.Equal(t, tidHex, hex.EncodeToString(stub.depeered[0][:]))
}

func TestPeersRemove_OtherError_500(t *testing.T) {
	stub := &stubFederation{depeerErr: errors.New("boom")}
	srv := newTestServer(t, func(d *Deps) { d.Federation = stub })
	defer srv.Close()

	body := `{"tracker_id":"` + strings.Repeat("ab", 32) + `"}`
	resp := adminReq(t, "POST", srv.URL+"/peers/remove", "test-token", body)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
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
