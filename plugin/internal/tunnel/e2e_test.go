package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_ConsumerSeederRoundTrip runs a full request/response cycle
// over a real loopback QUIC connection and asserts byte-for-byte
// integrity of both the request and the streamed response.
func TestE2E_ConsumerSeederRoundTrip(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv, PeerPin: consumerPub, Now: clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.LocalAddr()

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello \"}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"world\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	requestBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

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
		// Do NOT defer seederTun.Close() here. Close calls
		// CloseWithError on the underlying QUIC connection, which
		// would race the consumer's io.ReadAll(r) and surface as
		// "Application error 0x0 (remote): tunnel closed". CloseWrite
		// below is the half-duplex EOF signal; outer defers
		// (ln.Close + consumerTun.Close) tear down the connection
		// after the consumer has finished draining.

		body, err := seederTun.ReadRequest()
		if err != nil {
			seederErr = err
			return
		}
		assert.Equal(t, requestBody, body)

		if err := seederTun.SendOK(); err != nil {
			seederErr = err
			return
		}
		w := seederTun.ResponseWriter()
		for _, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				seederErr = err
				return
			}
		}
		seederErr = seederTun.CloseWrite()
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumerTun, err := Dial(dialCtx, addr, Config{
		EphemeralPriv: consumerPriv, PeerPin: seederPub, Now: clk,
	})
	require.NoError(t, err)
	defer consumerTun.Close()

	require.NoError(t, consumerTun.Send(requestBody))
	st, r, err := consumerTun.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, statusOK, st)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, strings.Join(chunks, ""), string(got))

	wg.Wait()
	require.NoError(t, seederErr)
}
