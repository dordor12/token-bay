package seederflow_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

// stubTunnel implements seederflow.TunnelConn for tests.
type stubTunnel struct {
	body        []byte
	readErr     error
	sentOK      bool
	sentErr     string
	respBuf     bytes.Buffer
	closedWrite bool
	closed      bool
	sendOKErr   error
	sendErrErr  error
}

func (s *stubTunnel) ReadRequest() ([]byte, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.body, nil
}

func (s *stubTunnel) SendOK() error              { s.sentOK = true; return s.sendOKErr }
func (s *stubTunnel) ResponseWriter() io.Writer  { return &s.respBuf }
func (s *stubTunnel) SendError(msg string) error { s.sentErr = msg; return s.sendErrErr }
func (s *stubTunnel) CloseWrite() error          { s.closedWrite = true; return nil }
func (s *stubTunnel) Close() error               { s.closed = true; return nil }

// makeAnthropicBody returns a minimal /v1/messages JSON body.
func makeAnthropicBody(t *testing.T, model, prompt string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": prompt,
			},
		},
	})
	require.NoError(t, err)
	return body
}

func TestServe_BridgeInvokedWithRequest(t *testing.T) {
	cfg := validConfig(t)
	bridge := &stubBridge{}
	cfg.Bridge = bridge
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.True(t, dec.Accept)

	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	require.NoError(t, c.Serve(context.Background(), tn, o.EnvelopeHash))
	require.Equal(t, 1, bridge.calls)
	require.True(t, tn.sentOK)
	require.True(t, tn.closedWrite)
}

func TestServe_RejectsUnknownReservation(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	var unknown [32]byte
	for i := range unknown {
		unknown[i] = 0xff
	}
	err = c.Serve(context.Background(), tn, unknown)
	require.ErrorIs(t, err, seederflow.ErrNoReservation)
	require.NotEmpty(t, tn.sentErr)
}

func TestServe_ConsumesReservationOnce(t *testing.T) {
	c, err := seederflow.New(validConfig(t))
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	_, err = c.HandleOffer(context.Background(), o)
	require.NoError(t, err)

	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	require.NoError(t, c.Serve(context.Background(), tn, o.EnvelopeHash))
	require.False(t, c.HasReservation(o.EnvelopeHash), "reservation must be consumed")

	tn2 := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	err = c.Serve(context.Background(), tn2, o.EnvelopeHash)
	require.ErrorIs(t, err, seederflow.ErrNoReservation)
}

func TestServe_TranslatesToSSE(t *testing.T) {
	cfg := validConfig(t)
	cfg.Bridge = &realStreamBridge{}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	_, err = c.HandleOffer(context.Background(), o)
	require.NoError(t, err)

	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	require.NoError(t, c.Serve(context.Background(), tn, o.EnvelopeHash))
	got := tn.respBuf.String()
	require.Contains(t, got, "event: message_start", "SSE must include message_start envelope")
	require.Contains(t, got, "event: message_stop", "SSE must include terminal message_stop")
	require.Contains(t, got, "content_block_delta", "text must arrive as a delta")
}

func TestServe_SendsUsageReport(t *testing.T) {
	cfg := validConfig(t)
	tracker := &stubTracker{}
	cfg.Tracker = tracker
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	_, err = c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	require.NoError(t, c.Serve(context.Background(), tn, o.EnvelopeHash))
	require.Len(t, tracker.UsageReports(), 1)
	ur := tracker.UsageReports()[0]
	require.Equal(t, "claude-sonnet-4-6", ur.Model)
	require.EqualValues(t, 10, ur.InputTokens)
	require.EqualValues(t, 20, ur.OutputTokens)
	require.NotEmpty(t, ur.SeederSig)
}

func TestServe_AuditEntryWritten(t *testing.T) {
	cfg := validConfig(t)
	audit := &stubAuditLog{}
	cfg.AuditLog = audit
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	_, err = c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	require.NoError(t, c.Serve(context.Background(), tn, o.EnvelopeHash))
	require.Len(t, audit.Records(), 1)
	require.Equal(t, "claude-sonnet-4-6", audit.Records()[0].Model)
	require.Equal(t, 10, audit.Records()[0].InputTokens)
	require.Equal(t, 20, audit.Records()[0].OutputTokens)
	require.NotEqual(t, [32]byte{}, audit.Records()[0].ConsumerIDHash, "audit must record consumer_id_hash")
}

func TestServe_ActiveClientLifecycle(t *testing.T) {
	cfg := validConfig(t)
	gate := newGate()
	stage := &gatedBridge{stage: gate}
	cfg.Bridge = stage
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	_, err = c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	done := make(chan error, 1)
	go func() { done <- c.Serve(context.Background(), tn, o.EnvelopeHash) }()
	stage.waitEntered()
	require.True(t, c.IsActiveByHash(stage.lastClientHash()), "must be active during bridge call")
	gate.release()
	require.NoError(t, <-done)
	require.False(t, c.IsActiveByHash(stage.lastClientHash()), "must clear active after Serve returns")
}

// realStreamBridge writes a real Claude stream-json message to sink so
// the ssetranslate adapter has something to translate.
type realStreamBridge struct{}

func (realStreamBridge) Serve(_ context.Context, _ ccbridge.Request, sink io.Writer) (ccbridge.Usage, error) {
	if sink != nil {
		assistant := `{"type":"assistant","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi back"}]}}` + "\n"
		result := `{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}` + "\n"
		if _, err := io.WriteString(sink, assistant); err != nil {
			return ccbridge.Usage{}, err
		}
		if _, err := io.WriteString(sink, result); err != nil {
			return ccbridge.Usage{}, err
		}
	}
	return ccbridge.Usage{InputTokens: 10, OutputTokens: 20}, nil
}

// gate is a one-shot lifecycle helper: waitEntered() blocks until fire()
// is called, release() lets the bridge return.
type gate struct {
	entered  chan struct{}
	released chan struct{}
	once     sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), released: make(chan struct{})}
}

func (g *gate) fire() {
	g.once.Do(func() { close(g.entered) })
}
func (g *gate) waitEntered() { <-g.entered }
func (g *gate) release()     { close(g.released) }

type gatedBridge struct {
	stage      *gate
	clientHash string
}

func (g *gatedBridge) Serve(ctx context.Context, req ccbridge.Request, sink io.Writer) (ccbridge.Usage, error) {
	g.clientHash = ccbridge.ClientHash(req.ClientPubkey)
	g.stage.fire()
	select {
	case <-g.stage.released:
	case <-ctx.Done():
		return ccbridge.Usage{}, ctx.Err()
	}
	if sink != nil {
		_, _ = io.WriteString(sink, `{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}`+"\n")
	}
	return ccbridge.Usage{InputTokens: 10, OutputTokens: 20}, nil
}

func (g *gatedBridge) waitEntered()           { g.stage.waitEntered() }
func (g *gatedBridge) lastClientHash() string { return g.clientHash }
