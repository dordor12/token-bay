package ccproxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/tunnel"
)

// TestNetworkDialer_E2E_RealTunnelLoopback runs a full Route through a
// real tunnel.Listener with a hand-rolled fake seeder goroutine. No
// real claude binary, no SSE translation — just byte-level wire.
func TestNetworkDialer_E2E_RealTunnelLoopback(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	ln, err := tunnel.Listen(netip.MustParseAddrPort("127.0.0.1:0"), tunnel.Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	canned := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	requestBody := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	var seederErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seederTun, err := ln.Accept(ctx)
		if err != nil {
			seederErr = err
			return
		}
		// Do not Close — see tunnel/e2e_test.go: CloseWrite is the half-
		// duplex EOF signal. Test cleanup tears down the listener.

		body, err := seederTun.ReadRequest()
		if err != nil {
			seederErr = err
			return
		}
		if !bytes.Equal(body, requestBody) {
			seederErr = io.ErrUnexpectedEOF
			return
		}
		if err := seederTun.SendOK(); err != nil {
			seederErr = err
			return
		}
		w := seederTun.ResponseWriter()
		if _, err := io.WriteString(w, canned); err != nil {
			seederErr = err
			return
		}
		seederErr = seederTun.CloseWrite()
	}()

	dialer := &tunnelDialer{Now: clk}
	router := &NetworkRouter{Dialer: dialer}

	meta := &EntryMetadata{
		SeederAddr:    ln.LocalAddr(),
		SeederPubkey:  seederPub,
		EphemeralPriv: consumerPriv,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(requestBody)))
	rec := httptest.NewRecorder()

	router.Route(rec, req, meta)

	wg.Wait()
	require.NoError(t, seederErr)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, canned, rec.Body.String())
}

// TestNetworkDialer_RejectsZeroEntryMetadata asserts the validation
// gate before tunnel.Dial is reached.
func TestNetworkDialer_RejectsZeroEntryMetadata(t *testing.T) {
	d := &tunnelDialer{}
	_, err := d.Dial(context.Background(), &EntryMetadata{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoAssignment)
}
