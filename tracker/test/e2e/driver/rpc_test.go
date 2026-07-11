//go:build e2e

package driver

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// --- frame codec: pure round-trip, no live tracker -----------------------

func TestFrameCodec_RoundTrip(t *testing.T) {
	req := &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE,
		Payload: []byte{0x01, 0x02, 0x03},
	}
	var buf bytes.Buffer
	require.NoError(t, writeFrame(&buf, req, maxBalanceFrameSize))

	var got tbproto.RpcRequest
	require.NoError(t, readFrame(&buf, &got, maxBalanceFrameSize))
	assert.Equal(t, req.Method, got.Method)
	assert.Equal(t, req.Payload, got.Payload)
	assert.Zero(t, buf.Len(), "round trip should consume the whole frame")
}

func TestFrameCodec_WriteRejectsOversize(t *testing.T) {
	req := &tbproto.RpcRequest{Payload: make([]byte, 128)}
	var buf bytes.Buffer
	err := writeFrame(&buf, req, 16)
	require.ErrorIs(t, err, errFrameTooLarge)
	assert.Zero(t, buf.Len(), "no bytes should hit the wire on an oversize marshal")
}

func TestFrameCodec_ReadRejectsOversizeHeader(t *testing.T) {
	// A 4-byte header declaring 2 MiB, nothing else — readFrame must
	// reject on the declared length alone without waiting for a body.
	frame := []byte{0x00, 0x20, 0x00, 0x00}
	var got tbproto.RpcResponse
	err := readFrame(bytes.NewReader(frame), &got, maxBalanceFrameSize)
	require.ErrorIs(t, err, errFrameTooLarge)
}

// --- clientIdentityID: must match the server's own derivation ------------

// TestClientIdentityID_MatchesServerDerivation locks the driver's
// pure-function identity derivation (sha256 over DER SPKI) to the exact
// value the tracker assigns a connecting peer: server.CertFromIdentity
// builds the client cert and server.SPKIToIdentityID hashes its SPKI —
// the driver must predict that ID without a live connection.
func TestClientIdentityID_MatchesServerDerivation(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)

	got, err := clientIdentityID(priv)
	require.NoError(t, err)

	tlsCert, err := server.CertFromIdentity(priv)
	require.NoError(t, err)
	require.NotEmpty(t, tlsCert.Certificate)
	parsed, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)
	want, err := server.SPKIToIdentityID(parsed)
	require.NoError(t, err)

	assert.Equal(t, want[:], got[:], "driver clientIdentityID must equal server SPKIToIdentityID for the same key")

	// Guard against accidental tls.Certificate misuse elsewhere.
	var _ tls.Certificate = tlsCert
}
