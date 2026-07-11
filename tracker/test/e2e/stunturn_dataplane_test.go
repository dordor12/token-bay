//go:build e2e

// Scenario 36: the tracker's STUN/TURN data plane, exercised host-side.
// Proves the :3478 reflector and :3479 token-framed relay work end to end
// against real sockets, independent of the plugin tunnel.
package e2e_test

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

func TestScenario36_StunTurnDataPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// STUN: a binding request returns a valid reflexive address.
	reflexive, err := driver.STUNReflexive(ctx, "localhost:3478")
	require.NoError(t, err, "STUN binding request to tracker :3478")
	assert.True(t, reflexive.IsValid(), "reflected XOR-MAPPED-ADDRESS must be a valid AddrPort, got %v", reflexive)
	t.Logf("e2e: scenario 36: STUN reflexive = %v", reflexive)

	// TURN: open a relay session and prove peer-to-peer copy.
	configureAndWaitForSeederAdvertise(ctx, t)
	cli := dialTrackerA(ctx, t)
	enrollClient(ctx, t, cli)
	sa := requestSeederAssignment(ctx, t, cli, 5, 5)

	resp, err := cli.Call(ctx, tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
		mustMarshal(t, &tbproto.TurnRelayOpenRequest{SessionId: sa.GetReservationToken()}))
	require.NoError(t, err)
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var turn tbproto.TurnRelayOpenResponse
	require.NoError(t, proto.Unmarshal(resp.Payload, &turn))
	token := turn.GetToken()
	require.Len(t, token, 16)

	peerA, err := net.Dial("udp", "localhost:3479")
	require.NoError(t, err)
	defer peerA.Close()
	peerB, err := net.Dial("udp", "localhost:3479")
	require.NoError(t, err)
	defer peerB.Close()

	// A sends (binds side A), then B sends (binds side B → forwards to A).
	_, _ = peerA.Write(driver.RelayFrame(token, []byte("ping-A")))
	time.Sleep(50 * time.Millisecond)
	_, _ = peerB.Write(driver.RelayFrame(token, []byte("ping-B")))

	buf := make([]byte, 1500)
	require.NoError(t, peerA.SetReadDeadline(time.Now().Add(3*time.Second)))
	n, err := peerA.Read(buf)
	require.NoError(t, err, "peer A must receive peer B's relayed datagram")
	require.GreaterOrEqual(t, n, 16, "relayed frame must carry the 16-byte token + payload")
	assert.Equal(t, token, driver.RelayToken(buf[:n]), "relayed frame keeps the session token")
	assert.Equal(t, []byte("ping-B"), buf[16:n], "relayed payload delivered verbatim")

	// Unknown token: dropped (no reply).
	bad := make([]byte, 16)
	bad[0] = 0xFF
	unknown, err := net.Dial("udp", "localhost:3479")
	require.NoError(t, err)
	defer unknown.Close()
	_, _ = unknown.Write(driver.RelayFrame(bad, []byte("nope")))
	require.NoError(t, unknown.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, rerr := unknown.Read(buf)
	assert.Error(t, rerr, "an unknown relay token gets no reply (dropped)")

	// Release the abandoned assignment's seeder load.
	reqIDHex := hex.EncodeToString(sa.GetReservationToken())
	_, _ = adminA().ForceFailInflight(ctx, reqIDHex)
	_, _ = adminA().ForceReleaseReservation(ctx, reqIDHex)
}
