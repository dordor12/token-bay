package seederflow_test

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

// recordingRunner satisfies ccbridge.Runner. It captures the Request
// it receives and emits a deterministic stream-json sequence so the
// real *ccbridge.Bridge yields a Usage and the ssetranslate adapter
// has SSE events to emit.
type recordingRunner struct {
	mu  sync.Mutex
	req ccbridge.Request
}

func (r *recordingRunner) Run(_ context.Context, req ccbridge.Request, sink io.Writer) error {
	r.mu.Lock()
	r.req = req
	r.mu.Unlock()
	if sink != nil {
		_, _ = io.WriteString(sink, `{"type":"system","subtype":"init","model":"`+req.Model+`"}`+"\n")
		_, _ = io.WriteString(sink, `{"type":"assistant","message":{"id":"msg_int","type":"message","role":"assistant","model":"`+req.Model+`","content":[{"type":"text","text":"hi back"}]}}`+"\n")
		_, _ = io.WriteString(sink, `{"type":"result","usage":{"input_tokens":13,"output_tokens":21}}`+"\n")
	}
	return nil
}

func (r *recordingRunner) Captured() ccbridge.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.req
}

// TestIntegration_OfferToUsageReport drives one offer through the
// coordinator with the real *ccbridge.Bridge (wrapping a recording
// runner) and the real *ssetranslate.Writer. Asserts:
//
//   - the runner received an airtight ccbridge.Request shape (model
//     pinned, ClientPubkey populated, last message is the user prompt)
//   - the resulting bytes on the tunnel are an Anthropic SSE envelope
//   - UsageReport was sent with bridge-extracted token counts
//   - the audit log entry recorded the consumer hash
func TestIntegration_OfferToUsageReport(t *testing.T) {
	cfg := validConfig(t)

	runner := &recordingRunner{}
	cfg.Bridge = ccbridge.NewBridge(runner)
	cfg.Runner = runner

	tracker := &stubTracker{}
	cfg.Tracker = tracker
	audit := &stubAuditLog{}
	cfg.AuditLog = audit
	acc := newStubAcceptor()
	cfg.Acceptor = acc

	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Push an offer and assert the coordinator accepts it.
	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(ctx, o)
	require.NoError(t, err)
	require.True(t, dec.Accept)
	require.Len(t, dec.EphemeralPubkey, 32)

	// Push a tunnel carrying an Anthropic /v1/messages body.
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hello seeder")}
	acc.conns <- tn

	// Wait for the usage report to land — that's the last step in Serve.
	require.Eventually(t, func() bool { return len(tracker.UsageReports()) == 1 }, 2*time.Second, 10*time.Millisecond)

	// Runner saw the request with the right model and a populated ClientPubkey.
	captured := runner.Captured()
	require.Equal(t, "claude-sonnet-4-6", captured.Model)
	require.Len(t, captured.ClientPubkey, 32)
	require.NotEmpty(t, captured.Messages)
	require.Equal(t, ccbridge.RoleUser, captured.Messages[len(captured.Messages)-1].Role)

	// And the airtight argv contract holds: BuildArgv produces the
	// pinned tool-disabling flags. Asserting against the real
	// ccbridge.BuildArgv keeps this test honest if the flag set ever
	// drifts.
	argv := ccbridge.BuildArgv(captured, "session-id", "hello seeder")
	require.True(t, slices.Contains(argv, ccbridge.FlagDisallowedTools))
	require.True(t, slices.Contains(argv, ccbridge.DisallowedToolsAll))
	require.True(t, slices.Contains(argv, ccbridge.FlagMCPConfig))
	require.True(t, slices.Contains(argv, ccbridge.MCPConfigNull))
	require.True(t, slices.Contains(argv, ccbridge.FlagSettings))
	require.True(t, slices.Contains(argv, ccbridge.SettingsNoHooks))
	require.True(t, slices.Contains(argv, ccbridge.FlagStrictMCPConfig))
	require.True(t, slices.Contains(argv, ccbridge.FlagNoSessionPersistence))

	// SSE bytes flowed through the translator.
	out := tn.respBuf.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, "event: content_block_delta")
	require.Contains(t, out, "event: message_stop")

	// UsageReport carries the token counts emitted by stream-json.
	ur := tracker.UsageReports()[0]
	require.Equal(t, "claude-sonnet-4-6", ur.Model)
	require.EqualValues(t, 13, ur.InputTokens)
	require.EqualValues(t, 21, ur.OutputTokens)
	require.NotEmpty(t, ur.SeederSig)

	// Audit log captured the consumer hash and the bridge-reported tokens.
	recs := audit.Records()
	require.Len(t, recs, 1)
	require.Equal(t, 13, recs[0].InputTokens)
	require.Equal(t, 21, recs[0].OutputTokens)
	require.NotEqual(t, [32]byte{}, recs[0].ConsumerIDHash)

	cancel()
	require.NoError(t, <-runDone)
}
