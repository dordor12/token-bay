//go:build e2e

package e2e_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// TestScenario01_HealthAndTopology is scenario 1 of the plan's 20-scenario
// suite (docs/superpowers/plans/2026-07-08-tracker-e2e-testing.md Task 24
// Step 2): both trackers report healthy, the federation handshake between
// them settles to "steady", and tracker-a's startup ledger-integrity gate
// ran and passed.
func TestScenario01_HealthAndTopology(t *testing.T) {
	ctx := context.Background()

	ha, err := adminA().Health(ctx)
	require.NoError(t, err, "tracker-a /health")
	assert.Equal(t, "ok", ha.Status)

	hb, err := adminB().Health(ctx)
	require.NoError(t, err, "tracker-b /health")
	assert.Equal(t, "ok", hb.Status)

	// Federation handshake + root-attestation exchange happens
	// asynchronously after bring-up, so tracker-b may not appear
	// "steady" in tracker-a's peer list the instant /health goes green.
	// Poll rather than sleep a fixed duration.
	var lastPeers *driver.PeersResponse
	eventually(t, 30*time.Second, 2*time.Second, `tracker-a /peers to show tracker-b in state "steady"`, func() bool {
		peers, err := adminA().Peers(ctx)
		if err != nil {
			return false
		}
		lastPeers = peers
		for _, p := range peers.Peers {
			if p.State == "steady" {
				return true
			}
		}
		return false
	})
	if lastPeers != nil {
		t.Logf("tracker-a /peers at pass time: %+v", lastPeers.Peers)
	}

	logs, err := compose().Logs("tracker-a")
	require.NoError(t, err, "docker compose logs tracker-a")
	assert.Contains(t, logs, "ledger integrity verified at startup")
}

// TestScenario02_EnrollmentAndStarterGrant is scenario 2 of the plan's
// suite (Task 24 Step 3): the consumer and seeder actors each enrolled
// with tracker-a and received a distinct, non-zero identity plus the
// 1000-credit starter grant, and the ledger tip advanced to record both
// grants.
func TestScenario02_EnrollmentAndStarterGrant(t *testing.T) {
	ctx := context.Background()

	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err, "consumer /identity")
	assertNonZeroHexIdentity(t, "consumer", consumerID.IdentityIDHex)

	seederID, err := seederCtl().Identity(ctx)
	require.NoError(t, err, "seeder /identity")
	assertNonZeroHexIdentity(t, "seeder", seederID.IdentityIDHex)

	require.NotEqual(t, consumerID.IdentityIDHex, seederID.IdentityIDHex, "consumer and seeder should be distinct identities")

	consumerIdentity := waitForStarterGrant(ctx, t, "consumer", consumerID.IdentityIDHex)
	assert.EqualValues(t, 1000, consumerIdentity.Balance.Credits)

	seederIdentity := waitForStarterGrant(ctx, t, "seeder", seederID.IdentityIDHex)
	assert.EqualValues(t, 1000, seederIdentity.Balance.Credits)

	var stats *driver.StatsResponse
	eventually(t, 15*time.Second, 1*time.Second, "tracker-a /stats ledger.tip_seq to reach >= 2 (both starter grants)", func() bool {
		s, err := adminA().Stats(ctx)
		if err != nil || s.Ledger.TipSeq == nil {
			return false
		}
		stats = s
		return *s.Ledger.TipSeq >= 2
	})
	require.NotNil(t, stats)
	require.NotNil(t, stats.Ledger.TipSeq, "ledger tip_seq should be non-nil once entries exist")
	assert.GreaterOrEqual(t, *stats.Ledger.TipSeq, uint64(2))
}

// assertNonZeroHexIdentity fails the test unless idHex is exactly 64 hex
// characters (a 32-byte identity) and not the all-zero identity.
func assertNonZeroHexIdentity(t *testing.T, who, idHex string) {
	t.Helper()
	require.Len(t, idHex, 64, "%s identity_id_hex should be 64 hex chars (32 bytes)", who)
	_, err := hex.DecodeString(idHex)
	require.NoError(t, err, "%s identity_id_hex should be valid hex", who)
	assert.NotEqual(t, strings.Repeat("0", 64), idHex, "%s identity_id_hex should not be the all-zero identity", who)
}

// waitForStarterGrant polls Admin(a).Identity(idHex) until the registry
// reports a signed balance snapshot for it (the starter grant is written
// asynchronously relative to the actor's own enrollment call returning),
// then returns the identity response for the caller to assert on.
func waitForStarterGrant(ctx context.Context, t *testing.T, who, idHex string) *driver.IdentityResponse {
	t.Helper()
	var identity *driver.IdentityResponse
	eventually(t, 15*time.Second, 1*time.Second, who+" starter grant to land (Admin(a).Identity balance populated)", func() bool {
		id, err := adminA().Identity(ctx, idHex)
		if err != nil || id.Balance == nil {
			return false
		}
		identity = id
		return true
	})
	require.NotNil(t, identity, "%s: Admin(a).Identity never returned a balance", who)
	require.NotNil(t, identity.Balance, "%s: identity balance", who)
	return identity
}
