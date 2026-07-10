package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/harness"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// TestActor_HealthzAfterEnroll boots the actor lifecycle against an
// in-process fakeserver tracker and asserts that once the actor has
// connected and enrolled, /healthz reports 200 and /identity returns the
// tracker-issued identity id. Non-vacuous: the fakeserver must actually
// receive the Enroll RPC.
func TestActor_HealthzAfterEnroll(t *testing.T) {
	const addr = "tracker-a:0"

	// Tracker-issued SPKI-hash identity, distinct from the client's raw
	// pubkey hash — mirrors production where the enroll id is the mTLS
	// SPKI hash, not sha256(rawPubkey).
	wantID := make([]byte, 32)
	for i := range wantID {
		wantID[i] = byte(0xB0 + i%16)
	}

	srv, transport := harness.Loopback(addr)

	var enrollCalls atomic.Int32
	srv.Handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = func(_ context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
		er, ok := req.(*tbproto.EnrollRequest)
		require.True(t, ok, "enroll handler got %T", req)
		// The tracker only checks lengths; assert the actor sent a
		// well-formed payload.
		assert.Len(t, er.IdentityPubkey, 32)
		assert.Len(t, er.AccountFingerprint, 32)
		assert.Equal(t, RoleConsumer, er.Role)
		enrollCalls.Add(1)
		return tbproto.RpcStatus_RPC_STATUS_OK, &tbproto.EnrollResponse{
			IdentityId:          wantID,
			StarterGrantCredits: 100,
		}, nil
	}

	serverDone := make(chan struct{})
	go func() {
		_ = srv.Run(context.Background())
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = srv.Conn.Close()
		<-serverDone
	})

	var trackerHash [32]byte
	trackerHash[0] = 0x11 // any non-zero value; loopback ignores the pin

	actor, err := newActor(options{
		Role:        RoleConsumer,
		RoleName:    "consumer",
		TrackerAddr: addr,
		TrackerHash: trackerHash,
		Region:      "A",
		DataDir:     t.TempDir(),
		CtrlAddr:    "127.0.0.1:0",
		Transport:   transport,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- actor.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Error("actor.run did not return after cancel")
		}
	})

	base := "http://" + actor.CtrlAddr()

	// Poll /healthz until it flips to 200 (connect + enroll complete).
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // test poll
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "/healthz never returned 200")

	require.Equal(t, int32(1), enrollCalls.Load(), "fakeserver did not receive exactly one Enroll RPC")

	// /identity returns the tracker-issued id (64 hex) and the pubkey.
	resp, err := http.Get(base + "/identity") //nolint:noctx // test request
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var idResp struct {
		IdentityIDHex string `json:"identity_id_hex"`
		PubkeyHex     string `json:"pubkey_hex"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&idResp))
	assert.Len(t, idResp.IdentityIDHex, 64, "identity_id_hex should be 64 hex chars")
	assert.Equal(t, hex.EncodeToString(wantID), idResp.IdentityIDHex,
		"identity_id_hex must be the enroll-returned id, not sha256(rawPubkey)")
	assert.Len(t, idResp.PubkeyHex, 64, "pubkey_hex should be 64 hex chars")
}
