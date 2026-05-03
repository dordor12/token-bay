package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestWriteThenReadRoundTrip(t *testing.T) {
	in := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BALANCE, Payload: []byte("abc")}
	var buf bytes.Buffer
	require.NoError(t, Write(&buf, in, 1<<10))

	out := &tbproto.RpcRequest{}
	require.NoError(t, Read(&buf, out, 1<<10))
	assert.Equal(t, in.Method, out.Method)
	assert.Equal(t, in.Payload, out.Payload)
}

func TestWriteRejectsOversize(t *testing.T) {
	in := &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: make([]byte, 200),
	}
	err := Write(io.Discard, in, 64)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFrameTooLarge))
}

func TestReadRejectsOversizeHeader(t *testing.T) {
	// Hand-craft a frame whose declared length is 1 MB but max is 64.
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x10, 0x00, 0x00})
	err := Read(&buf, &tbproto.RpcRequest{}, 64)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFrameTooLarge))
}

func TestReadEOFBeforeHeader(t *testing.T) {
	var buf bytes.Buffer
	err := Read(&buf, &tbproto.RpcRequest{}, 64)
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadShortBody(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 10}) // claim 10 bytes
	buf.Write([]byte{1, 2, 3})     // give 3
	err := Read(&buf, &tbproto.RpcRequest{}, 1<<10)
	require.Error(t, err)
}

func TestSequentialFramesOnOneStream(t *testing.T) {
	a := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BALANCE, Payload: []byte("a")}
	b := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, Payload: []byte("bb")}
	var buf bytes.Buffer
	require.NoError(t, Write(&buf, a, 1<<10))
	require.NoError(t, Write(&buf, b, 1<<10))

	var out tbproto.RpcRequest
	require.NoError(t, Read(&buf, &out, 1<<10))
	assert.Equal(t, []byte("a"), out.Payload)
	out = tbproto.RpcRequest{}
	require.NoError(t, Read(&buf, &out, 1<<10))
	assert.Equal(t, []byte("bb"), out.Payload)
}
