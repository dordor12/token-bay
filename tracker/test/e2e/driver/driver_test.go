//go:build e2e

package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Admin: URL construction + JSON decoding (no live tracker) ----------

func TestAdmin_URLConstruction(t *testing.T) {
	a := NewAdmin("http://localhost:9090/", "tok")
	assert.Equal(t, "http://localhost:9090/health", a.url("/health"))
	assert.Equal(t, "http://localhost:9090/identity/deadbeef", a.url("/identity/deadbeef"))

	// Trailing slash on BaseURL is trimmed so paths never double up.
	assert.Equal(t, "http://localhost:9090", a.BaseURL)
}

func TestAdmin_Health_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/health", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": "dev",
			"time":    "2026-07-11T00:00:00Z",
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "test-token")
	h, err := a.Health(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", h.Status)
	assert.Equal(t, "dev", h.Version)
	assert.Equal(t, "2026-07-11T00:00:00Z", h.Time)
}

func TestAdmin_Stats_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/stats", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": 3,
			"ledger": map[string]any{
				"tip_seq":  42,
				"tip_hash": "aabbcc",
			},
			"merkle_root_minutes": 60,
			"broker_reqs_per_sec": nil,
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	s, err := a.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, s.Connections)
	require.NotNil(t, s.Ledger.TipSeq)
	assert.EqualValues(t, 42, *s.Ledger.TipSeq)
	require.NotNil(t, s.Ledger.TipHash)
	assert.Equal(t, "aabbcc", *s.Ledger.TipHash)
	assert.Equal(t, 60, s.MerkleRootMinutes)
	assert.Nil(t, s.BrokerReqsPerSec)
}

func TestAdmin_Stats_NullTip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": 0,
			"ledger": map[string]any{
				"tip_seq":  nil,
				"tip_hash": nil,
			},
			"merkle_root_minutes": 60,
			"broker_reqs_per_sec": nil,
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	s, err := a.Stats(context.Background())
	require.NoError(t, err)
	assert.Nil(t, s.Ledger.TipSeq)
	assert.Nil(t, s.Ledger.TipHash)
}

func TestAdmin_Identity_DecodesFixture(t *testing.T) {
	const idHex = "deadbeef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/identity/"+idHex, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"identity_id": idHex,
			"seeder": map[string]any{
				"available":         true,
				"headroom_estimate": 0.5,
				"reputation_score":  1.0,
				"load":              2,
				"last_heartbeat":    "2026-07-11T00:00:00Z",
				"models":            []string{"claude-sonnet-4-6"},
			},
			"balance": map[string]any{
				"credits":       1000,
				"chain_tip_seq": 7,
				"issued_at":     100,
				"expires_at":    700,
			},
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	id, err := a.Identity(context.Background(), idHex)
	require.NoError(t, err)
	assert.Equal(t, idHex, id.IdentityID)
	require.NotNil(t, id.Seeder)
	assert.True(t, id.Seeder.Available)
	assert.Equal(t, 2, id.Seeder.Load)
	assert.Equal(t, []string{"claude-sonnet-4-6"}, id.Seeder.Models)
	require.NotNil(t, id.Balance)
	assert.EqualValues(t, 1000, id.Balance.Credits)
	assert.EqualValues(t, 7, id.Balance.ChainTipSeq)
}

func TestAdmin_Identity_UnknownIsNotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"identity not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	_, err := a.Identity(context.Background(), "00")
	require.Error(t, err)
	var adminErr *AdminError
	require.ErrorAs(t, err, &adminErr)
	assert.Equal(t, http.StatusNotFound, adminErr.StatusCode)
}

func TestAdmin_Peers_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"peers": []map[string]any{
				{
					"tracker_id":   "aa",
					"pubkey":       "bb",
					"addr":         "tracker-b:7777",
					"region":       "us-east",
					"state":        "Steady",
					"health_score": 0.9,
				},
			},
			"connected_quic":   1,
			"federation_state": "enabled",
			"listen_addr":      "0.0.0.0:7778",
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	p, err := a.Peers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "enabled", p.FederationState)
	require.Len(t, p.Peers, 1)
	assert.Equal(t, "Steady", p.Peers[0].State)
	assert.Equal(t, "tracker-b:7777", p.Peers[0].Addr)
}

func TestAdmin_Freeze_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/identity/deadbeef/freeze", r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"identity_id": "deadbeef", "frozen": true})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	resp, err := a.Freeze(context.Background(), "deadbeef")
	require.NoError(t, err)
	assert.True(t, resp.Frozen)
}

func TestAdmin_BrokerInflight_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/broker/inflight/abc123", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id":         "abc123",
			"consumer_id":        "cc",
			"state":              "assigned",
			"seeder_id":          "dd",
			"reservation_amount": 500,
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	d, err := a.BrokerInflight(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Equal(t, "assigned", d.State)
	require.NotNil(t, d.ReservationAmount)
	assert.EqualValues(t, 500, *d.ReservationAmount)
}

func TestAdmin_Reservations_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/broker/reservations", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"consumer_id": "cc",
				"total":       500,
				"slots": []map[string]any{
					{"request_id": "abc123", "amount": 500, "expires_at": 1234567890},
				},
			},
		})
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	rs, err := a.Reservations(context.Background())
	require.NoError(t, err)
	require.Len(t, rs, 1)
	assert.Equal(t, "cc", rs[0].ConsumerID)
	require.Len(t, rs[0].Slots, 1)
	assert.EqualValues(t, 500, rs[0].Slots[0].Amount)
}

func TestAdmin_NonJSONErrorBody_SurfacesRawText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reputation subsystem not configured", http.StatusNotImplemented)
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL, "tok")
	_, err := a.Freeze(context.Background(), "deadbeef")
	require.Error(t, err)
	var adminErr *AdminError
	require.ErrorAs(t, err, &adminErr)
	assert.Equal(t, http.StatusNotImplemented, adminErr.StatusCode)
	assert.Contains(t, adminErr.Body, "reputation subsystem not configured")
}

// --- Compose: pure arg-vector builder (no live docker) -------------------

func TestCompose_UpArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "up", "-d", "--build"}, c.UpArgs())
}

func TestCompose_UpArgs_WithProject(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	assert.Equal(t,
		[]string{"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e", "up", "-d", "--build"},
		c.UpArgs())
}

func TestCompose_DownArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "down"}, c.DownArgs(false))
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "down", "-v"}, c.DownArgs(true))
}

func TestCompose_ExecArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	got := c.ExecArgs("tracker-a", "sqlite3", "/data/ledger.db", "select 1;")
	want := []string{
		"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e",
		"exec", "-T", "tracker-a", "sqlite3", "/data/ledger.db", "select 1;",
	}
	assert.Equal(t, want, got)
}

func TestCompose_ExecArgs_NoProject(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	got := c.ExecArgs("tracker-a", "echo", "hi")
	want := []string{"compose", "-f", "compose.e2e.yaml", "exec", "-T", "tracker-a", "echo", "hi"}
	assert.Equal(t, want, got)
}

func TestCompose_LogsArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "logs", "tracker-a"}, c.LogsArgs("tracker-a"))
}

func TestCompose_PsArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "ps"}, c.PsArgs())
}

func TestCompose_PsAllArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "ps", "-a"}, c.PsAllArgs())
}

func TestCompose_PsAllArgs_WithProject(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	assert.Equal(t,
		[]string{"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e", "ps", "-a"},
		c.PsAllArgs())
}

func TestCompose_RestartArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	assert.Equal(t,
		[]string{"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e", "restart", "tracker-a"},
		c.RestartArgs("tracker-a"))
}

func TestCompose_StopArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "stop", "tracker-a"}, c.StopArgs("tracker-a"))
}

func TestCompose_StartArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	assert.Equal(t, []string{"compose", "-f", "compose.e2e.yaml", "start", "tracker-a"}, c.StartArgs("tracker-a"))
}

func TestCompose_KillArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	assert.Equal(t,
		[]string{"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e", "kill", "-s", "SIGTERM", "tracker-a"},
		c.KillArgs("tracker-a", "SIGTERM"))
}

func TestCompose_RunArgs_NoEntrypoint(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	got := c.RunArgs("tracker-a", "")
	want := []string{"compose", "-f", "compose.e2e.yaml", "run", "--rm", "-T", "tracker-a"}
	assert.Equal(t, want, got)
}

func TestCompose_RunArgs_WithEntrypointAndArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml", Project: "tokenbay-e2e"}
	got := c.RunArgs("tracker-a", "sh", "-c", "echo hi")
	want := []string{
		"compose", "-f", "compose.e2e.yaml", "-p", "tokenbay-e2e",
		"run", "--rm", "-T", "--entrypoint", "sh", "tracker-a", "-c", "echo hi",
	}
	assert.Equal(t, want, got)
}

// ArgsBuilder calls must not alias/mutate each other's backing arrays —
// regression guard for a subtle append() bug class.
func TestCompose_ArgsBuilders_DoNotAlias(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	up := c.UpArgs()
	down := c.DownArgs(false)
	up[0] = "MUTATED"
	assert.Equal(t, "compose", down[0], "DownArgs must not observe a mutation of UpArgs's backing array")
}

// --- actors: URL + JSON decoding (no live actor binaries) ----------------

func TestConsumerCtl_Identity_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/identity", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"identity_id_hex": "aa11",
			"pubkey_hex":      "bb22",
		})
	}))
	defer srv.Close()

	c := NewConsumerCtl(srv.URL)
	id, err := c.Identity(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "aa11", id.IdentityIDHex)
	assert.Equal(t, "bb22", id.PubkeyHex)
}

func TestConsumerCtl_Request_PostsBodyAndDecodesResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/request", r.URL.Path)
		var got RequestSpec
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		assert.Equal(t, "claude-sonnet-4-6", got.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"outcome":               "seeder_assignment",
			"seeder_addr":           "seeder:9999",
			"seeder_pubkey_hex":     "cc33",
			"reservation_token_hex": "dd44",
		})
	}))
	defer srv.Close()

	c := NewConsumerCtl(srv.URL)
	res, err := c.Request(context.Background(), RequestSpec{Model: "claude-sonnet-4-6", MaxInputTokens: 100, MaxOutputTokens: 50})
	require.NoError(t, err)
	assert.Equal(t, "seeder_assignment", res.Outcome)
	assert.Equal(t, "seeder:9999", res.SeederAddr)
}

func TestSeederCtl_LastOffer_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/offers/last", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"consumer_id_hex":      "ee55",
			"request_id_hex":       "ff66",
			"model":                "claude-sonnet-4-6",
			"accepted":             true,
			"ephemeral_pubkey_hex": "1122",
		})
	}))
	defer srv.Close()

	c := NewSeederCtl(srv.URL)
	off, err := c.LastOffer(context.Background())
	require.NoError(t, err)
	assert.True(t, off.Accepted)
	assert.Equal(t, "ff66", off.RequestIDHex)
}

func TestFedactorCtl_Received_DecodesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/received", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		// Field names match the real fedactor's recvRecord
		// (cmd/fedactor/actor.go): {kind, sender_id, at}.
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"kind": "KIND_EQUIVOCATION_EVIDENCE", "sender_id": "aa", "at": "2026-07-11T00:00:00Z"},
		})
	}))
	defer srv.Close()

	f := NewFedactorCtl(srv.URL)
	got, err := f.Received(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "KIND_EQUIVOCATION_EVIDENCE", got[0].Kind)
	assert.Equal(t, "aa", got[0].SenderID)
	assert.Equal(t, "2026-07-11T00:00:00Z", got[0].ReceivedAt)
}

// TestFedactorCtl_Handshake_PostsBody locks in the field names/JSON tags
// against the real fedactor's handshakeReq (cmd/fedactor/control.go):
// {addr, pubkey_hex} — NOT the originally-guessed
// target_addr/target_pub_hex, which the real handler would have
// silently decoded as zero-valued (json.Decoder ignores unknown
// fields by default).
func TestFedactorCtl_Handshake_PostsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/handshake", r.URL.Path)
		var got map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		assert.Equal(t, "tracker-a:7443", got["addr"])
		assert.Equal(t, "deadbeef", got["pubkey_hex"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFedactorCtl(srv.URL)
	err := f.Handshake(context.Background(), HandshakeSpec{Addr: "tracker-a:7443", PubKeyHex: "deadbeef"})
	require.NoError(t, err)
}

func TestFedactorCtl_SendRevocation_PostsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/send/revocation", r.URL.Path)
		var got RevocationSpec
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		assert.Equal(t, "deadbeef", got.IdentityHex)
		assert.Equal(t, 1, got.Reason)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFedactorCtl(srv.URL)
	err := f.SendRevocation(context.Background(), RevocationSpec{IdentityHex: "deadbeef", Reason: 1})
	require.NoError(t, err)
}

// --- sqlite: delegates to Compose.Exec with the right argv ---------------

func TestSQLiteQuery_BuildsExpectedArgs(t *testing.T) {
	c := Compose{File: "compose.e2e.yaml"}
	got := c.ExecArgs("tracker-a", "sqlite3", "/data/ledger.db", "select * from entries;")
	want := []string{
		"compose", "-f", "compose.e2e.yaml",
		"exec", "-T", "tracker-a", "sqlite3", "/data/ledger.db", "select * from entries;",
	}
	assert.Equal(t, want, got)
}
