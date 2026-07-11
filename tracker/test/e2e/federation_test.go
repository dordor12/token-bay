//go:build e2e

// Scenarios 11-13 of the plan's suite (Task 27): federation exchange
// between the two REAL trackers, plus the Byzantine fedactor neighbor
// (tracker/test/e2e/cmd/fedactor). All three share the SAME long-running
// compose stack as every other scenario file in this package
// (main_test.go's TestMain) so none may leave tracker-a/tracker-b in a
// state that breaks a later scenario or a subsequent full-suite run —
// scenario 13 in particular freezes a THROWAWAY, fabricated identity
// rather than the shared consumer/seeder actors.
//
// # Why scenario 11 seeds merkle_roots directly via SQL
//
// The plan's Task 27 Step 1 blurb says "wait for a publish_cadence_s
// tick" and assumes that's sufficient for tracker-a and tracker-b to
// exchange ROOT_ATTESTATIONs. Empirically tracing the real
// implementation (verified by reading, not assumed) shows that alone
// can never happen inside a short-lived e2e run:
//
//   - cmd/token-bay-tracker/maintenance.go's startFederationPublisher
//     ticks every cfg.Federation.PublishCadenceS (e2egen clamps this to
//     60s, the config.Validate floor) and calls
//     Federation.PublishHour(hour-1) — hour = floor(now/3600), i.e. the
//     literal, real-wall-clock PREVIOUS hour bucket.
//   - PublishHour silently no-ops (Publisher.PublishHour, ReadyRoot
//     returns ok=false) unless the ledger already has a persisted
//     merkle_roots row for that exact hour.
//   - That row is normally produced by Ledger.StartRollup
//     (internal/ledger/rollup.go), whose OWN ticker fires only every
//     cfg.Ledger.MerkleRootIntervalMin (default 60 MINUTES,
//     un-tuned by e2egen — config.Validate only requires > 0, but
//     e2egen doesn't lower it), and even then only computes a root for
//     entries timestamped in that same closed real-hour bucket
//     (internal/ledger/storage/merkle.go's hourSeconds = 3600,
//     hardcoded, no config knob). A freshly-provisioned e2e stack has
//     zero ledger activity in any FULLY CLOSED real hour before "now" —
//     every entry any scenario produces lands in the CURRENT
//     (still-open) hour bucket — so RunRollupOnce's target hour is
//     always empty and never produces a row within a run measured in
//     seconds, regardless of run duration short of an actual wall-clock
//     hour rollover.
//
// This is not a product bug: hourly Merkle rollups are a reasonable
// production design, just not observable end-to-end in under an hour
// without a test-mode clock override (none exists). Scenario 11 below
// borrows the exact technique ledger_test.go's scenario 9 already
// established as legitimate for this suite — direct SQL manipulation of
// the tracker's own SQLite file — to seed each tracker's OWN
// merkle_roots row for the (real, currently-closed) target hour.
// tracker_sig is accepted as opaque content by the receiving side
// (internal/ledger/rollup.go's own doc comment, confirmed by reading
// internal/federation/rootattest.go's Apply: it never
// ed25519.Verifies RootAttestation.tracker_sig, only the OUTER envelope
// signature, which IS the tracker's real key doing real signing) so a
// random 32-byte root + random 64-byte sig is wire-valid. Everything
// downstream of that seed — the publisher tick picking it up, the
// envelope being freshly signed under each tracker's REAL federation
// identity key, the QUIC send, the peer's envelope-signature
// verification, and the PutPeerRoot archive write — is the genuine
// production code path this scenario exists to exercise.
package e2e_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// readGenFile reads a host-side e2egen artifact (e2eDir/.gen/name) —
// the SAME files bind-mounted read-only into every container at /gen —
// directly from the test binary. e2egen's output is a pure function of
// the fixed seeds in main_test.go, so this is deterministic and avoids
// a docker-exec round-trip for static key material scenarios need on
// the host side (raw pubkeys for FedactorCtl.Handshake, tracker_ids for
// SQL WHERE-clause filters).
func readGenFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e2eDir, ".gen", name))
	require.NoError(t, err, "read .gen/%s", name)
	return strings.TrimSpace(string(b))
}

// sqlCount runs a `SELECT count(*) ...` (or any single-integer-row)
// query against service's ledger DB and parses the result. Returns -1
// (never satisfies a "> 0" poll condition) on any exec/parse error,
// logging the cause — callers are always inside a pollUntilTrue loop,
// so a transient exec hiccup should retry, not fail the test outright.
func sqlCount(t *testing.T, service, query string) int {
	t.Helper()
	out, err := compose().Exec(service, "sqlite3", ledgerDBPath, query)
	if err != nil {
		t.Logf("e2e: sqlCount: %s exec error: %v", service, err)
		return -1
	}
	out = strings.TrimSpace(out)
	n, convErr := strconv.Atoi(out)
	if convErr != nil {
		t.Logf("e2e: sqlCount: unexpected sqlite3 output %q for query %q: %v", out, query, convErr)
		return -1
	}
	return n
}

// fetchMetricValue GETs url (a Prometheus text-exposition endpoint,
// e.g. tracker-a's :9100/metrics) and returns the value of the first
// unlabeled sample line whose metric name exactly matches name. ok=false
// means the metric line was never found (not yet registered, or the
// counter is still at its zero value and the client_golang registry
// omits it — not the case for plain Counters, which always emit their
// current value including 0, so ok=false here means something is
// actually wrong, not just "hasn't incremented yet").
func fetchMetricValue(ctx context.Context, url, name string) (value float64, ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false, fmt.Errorf("build metrics request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != name {
			continue
		}
		v, perr := strconv.ParseFloat(fields[1], 64)
		if perr != nil {
			return 0, false, fmt.Errorf("parse metric %s value %q: %w", name, fields[1], perr)
		}
		return v, true, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, false, fmt.Errorf("scan metrics body: %w", err)
	}
	return 0, false, nil
}

// TestScenario11_RootAttestationExchange is scenario 11 (Task 27 Step
// 1): the two REAL trackers gossip ROOT_ATTESTATIONs to each other and
// each archives the counterpart's row in peer_root_archive. See the
// package doc comment above for why this seeds each tracker's OWN
// merkle_roots row via direct SQL rather than waiting on a real
// wall-clock hour rollover.
func TestScenario11_RootAttestationExchange(t *testing.T) {
	trackerAFedIDHex := readGenFile(t, "tracker-a.fedid")
	trackerBFedIDHex := readGenFile(t, "tracker-b.fedid")
	require.Len(t, trackerAFedIDHex, 64)
	require.Len(t, trackerBFedIDHex, 64)

	// The just-closed real hour bucket, from the host's clock (assumed
	// to agree with the containers' — standard for local Docker, no
	// explicit clock skew is configured anywhere in compose.e2e.yaml).
	targetHour := uint64(time.Now().Unix())/3600 - 1 //nolint:gosec // G115 — post-1970, always positive
	require.Greater(t, targetHour, uint64(0))

	seedSQL := fmt.Sprintf(
		"INSERT OR IGNORE INTO merkle_roots (hour, root, tracker_sig) VALUES (%d, randomblob(32), randomblob(64));",
		targetHour,
	)
	_, err := compose().Exec("tracker-a", "sqlite3", ledgerDBPath, seedSQL)
	require.NoError(t, err, "seed tracker-a's own merkle_roots row for hour=%d", targetHour)
	_, err = compose().Exec("tracker-b", "sqlite3", ledgerDBPath, seedSQL)
	require.NoError(t, err, "seed tracker-b's own merkle_roots row for hour=%d", targetHour)

	// tracker-a's archive should gain a peer_root_archive row keyed by
	// tracker-b's fedid (and vice versa) for exactly this hour, once
	// each tracker's next PublishCadenceS tick (60s, e2egen-clamped)
	// picks up the seeded root and gossips it over the REAL, already
	// steady federation connection.
	queryOnA := fmt.Sprintf(
		"SELECT count(*) FROM peer_root_archive WHERE hex(tracker_id) = upper('%s') AND hour = %d;",
		trackerBFedIDHex, targetHour,
	)
	queryOnB := fmt.Sprintf(
		"SELECT count(*) FROM peer_root_archive WHERE hex(tracker_id) = upper('%s') AND hour = %d;",
		trackerAFedIDHex, targetHour,
	)

	// One combined poll (not two sequential 75s waits): a single
	// publish tick handles both directions since the connection is
	// already bidirectionally steady.
	require.True(t, pollUntilTrue(80*time.Second, 3*time.Second, func() bool {
		return sqlCount(t, "tracker-a", queryOnA) > 0 && sqlCount(t, "tracker-b", queryOnB) > 0
	}), "both trackers' peer_root_archive should gain the counterpart's root for hour=%d within ~75s (one publish_cadence_s tick)", targetHour)
}

// TestScenario12_EquivocationDepeer is scenario 12 (Task 27 Step 2): the
// Byzantine fedactor handshakes against tracker-a as a real,
// allowlisted federation peer (e2egen's cfgA includes the fedactor's
// identity, region "FED" — render.go's fedactorFederationAddr), then
// sends two ROOT_ATTESTATIONs for the SAME hour with DIFFERENT 32-byte
// roots. tracker-a's local-conflict path (internal/federation/rootattest.go's
// Apply -> storage.ErrPeerRootConflict -> Equivocator.OnLocalConflict)
// must depeer the fedactor (Registry.Depeer deletes it from the active
// map entirely — Peers() then either omits it or, if a reconnect
// somehow raced back in, shows it non-steady) and increment
// tokenbay_federation_equivocations_detected_total.
func TestScenario12_EquivocationDepeer(t *testing.T) {
	ctx := context.Background()

	trackerAPubHex := readGenFile(t, "tracker-a.pub")
	trackerBFedIDHex := readGenFile(t, "tracker-b.fedid")
	require.Len(t, trackerAPubHex, 64)

	metricBefore, hadMetricBefore, err := fetchMetricValue(ctx, metricsABaseURL+"/metrics", "tokenbay_federation_equivocations_detected_total")
	require.NoError(t, err)
	if !hadMetricBefore {
		metricBefore = 0
	}

	require.NoError(t, fedactorCtl().Handshake(ctx, driver.HandshakeSpec{
		Addr:      "tracker-a:7443",
		PubKeyHex: trackerAPubHex,
	}), "fedactor handshake against tracker-a")

	// The equivocation path only fires once the inbound handshake has
	// actually attached (Registry.Update sets state Steady) — wait for
	// that before sending anything.
	//
	// Matched by "the peer that ISN'T tracker-b", NOT Region=="FED":
	// verified live against the real stack that
	// Federation.attachPeerLocked's reg.Update call
	// (internal/federation/subsystem.go) only sets
	// {TrackerID, PubKey, Addr, State, Conn} — Region is NOT carried
	// over from the PeerStatePending row Open() seeded from config, so
	// every peer's Region reads back as "" once it reaches "steady".
	// tracker-a has exactly two configured peers (B and the fedactor,
	// per render.go's cfgA.Federation.Peers), so "not tracker-b's
	// fedid" unambiguously identifies the fedactor's row.
	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		return fedactorPeerStateOnA(ctx, t, trackerBFedIDHex) == "steady"
	}), "tracker-a should list the fedactor peer as steady after the handshake")

	const equivHour = uint64(424242)
	rootX := strings.Repeat("aa", 32) // 64 hex chars = 32 bytes
	rootY := strings.Repeat("bb", 32)

	require.NoError(t, fedactorCtl().SendRootAttestation(ctx, driver.RootAttestationSpec{Hour: equivHour, RootHex: rootX}))
	require.NoError(t, fedactorCtl().SendRootAttestation(ctx, driver.RootAttestationSpec{Hour: equivHour, RootHex: rootY}))

	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		state := fedactorPeerStateOnA(ctx, t, trackerBFedIDHex)
		return state != "steady" // absent (Depeer deletes) or non-steady
	}), "tracker-a should no longer list the fedactor peer as steady after the conflicting root attestations")

	var metricAfter float64
	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		v, ok, ferr := fetchMetricValue(ctx, metricsABaseURL+"/metrics", "tokenbay_federation_equivocations_detected_total")
		if ferr != nil || !ok {
			return false
		}
		metricAfter = v
		return true
	}), "tracker-a /metrics should expose tokenbay_federation_equivocations_detected_total")
	assert.Greater(t, metricAfter, metricBefore, "tokenbay_federation_equivocations_detected_total should have incremented")
}

// fedactorPeerStateOnA returns the "state" tracker-a's admin /peers
// reports for the fedactor's row — identified as whichever configured
// peer's tracker_id does NOT match trackerBFedIDHex, since tracker-a
// has exactly two configured peers (B and the fedactor) and Region
// isn't a reliable discriminator once a peer reaches "steady" (see the
// comment at this function's call site). Returns "" if the fedactor
// isn't listed at all (e.g. after Depeer, which deletes the entry from
// the registry map entirely) or on any /peers error — both cases the
// poll conditions above correctly treat as "not steady".
func fedactorPeerStateOnA(ctx context.Context, t *testing.T, trackerBFedIDHex string) string {
	t.Helper()
	peers, err := adminA().Peers(ctx)
	if err != nil {
		return ""
	}
	for _, p := range peers.Peers {
		if p.TrackerID != trackerBFedIDHex {
			return p.State
		}
	}
	return ""
}

// TestScenario13_RevocationPropagation is scenario 13 (Task 27 Step 3):
// an operator freeze on tracker-a (admin POST /identity/{id}/freeze)
// emits a signed REVOCATION that tracker-a self-archives
// (internal/federation/revocation.go's OnFreeze persists locally
// before forwarding) and gossips to tracker-b over the real,
// steady federation connection, landing in tracker-b's
// peer_revocations table.
//
// Uses a FABRICATED, throwaway 64-hex-char identity — never the shared
// consumer/seeder actors other scenarios in this long-running stack
// depend on staying unfrozen. reputation.Subsystem.Freeze's
// ensureState call means the identity need not have pre-existed in the
// registry.
func TestScenario13_RevocationPropagation(t *testing.T) {
	ctx := context.Background()

	const throwawayIDHex = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	require.Len(t, throwawayIDHex, 64)

	freezeResp, err := adminA().Freeze(ctx, throwawayIDHex)
	require.NoError(t, err, "admin(a) freeze throwaway identity")
	assert.True(t, freezeResp.Frozen)
	assert.Equal(t, throwawayIDHex, freezeResp.IdentityID)

	revocationQuery := fmt.Sprintf(
		"SELECT count(*) FROM peer_revocations WHERE hex(identity_id) = upper('%s');",
		throwawayIDHex,
	)

	// tracker-a self-archives synchronously inside OnFreeze, so this
	// should already be true almost immediately — poll defensively
	// rather than assume zero latency.
	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		return sqlCount(t, "tracker-a", revocationQuery) > 0
	}), "tracker-a should self-archive the REVOCATION it emitted on freeze")

	// Propagation to tracker-b via federation gossip.
	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		return sqlCount(t, "tracker-b", revocationQuery) > 0
	}), "tracker-b's peer_revocations should gain the freeze REVOCATION within 15s")
}
