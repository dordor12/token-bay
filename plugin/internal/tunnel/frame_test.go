package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteRequest_LengthPrefixed(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("hello world")
	require.NoError(t, writeRequest(&buf, body))

	require.Equal(t, 4+len(body), buf.Len())
	got := binary.BigEndian.Uint32(buf.Bytes()[:4])
	assert.Equal(t, uint32(len(body)), got)
	assert.Equal(t, body, buf.Bytes()[4:])
}

func TestReadRequest_HappyPath(t *testing.T) {
	body := []byte("the quick brown fox")
	var buf bytes.Buffer
	require.NoError(t, writeRequest(&buf, body))

	got, err := readRequest(&buf, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestReadRequest_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(1024))
	buf.Write(hdr[:])

	_, err := readRequest(&buf, 512)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRequestTooLarge))
}

func TestReadRequest_ShortHeader(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x00, 0x00})
	_, err := readRequest(r, 1<<20)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestReadRequest_TruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 16)
	buf.Write(hdr[:])
	buf.Write([]byte("only-7"))

	_, err := readRequest(&buf, 1<<20)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestWriteResponseStatus_OK(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeResponseStatus(&buf, statusOK))
	assert.Equal(t, []byte{0x00}, buf.Bytes())
}

func TestWriteResponseStatus_Error(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeResponseError(&buf, "out of credits"))
	require.Equal(t, byte(0x01), buf.Bytes()[0])
	assert.Equal(t, []byte("out of credits"), buf.Bytes()[1:])
}

func TestReadResponseStatus_OK(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 'a', 'b', 'c'})
	st, err := readResponseStatus(r)
	require.NoError(t, err)
	assert.Equal(t, statusOK, st)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("abc"), rest)
}

func TestReadResponseStatus_Error(t *testing.T) {
	r := bytes.NewReader(append([]byte{0x01}, []byte("rate limited")...))
	st, err := readResponseStatus(r)
	require.NoError(t, err)
	assert.Equal(t, statusError, st)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("rate limited"), rest)
}

func TestReadResponseStatus_UnknownByte(t *testing.T) {
	r := bytes.NewReader([]byte{0x42})
	_, err := readResponseStatus(r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestReadResponseStatus_EmptyStream(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := readResponseStatus(r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

// Augmenting the verbatim plan suite to lift frame.go coverage past the
// 90% bar. These exercise spec-documented edge cases (empty bodies, empty
// error messages) that the verbatim tests didn't enumerate.

func TestWriteRequest_EmptyBody(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRequest(&buf, nil))

	require.Equal(t, 4, buf.Len())
	assert.Equal(t, uint32(0), binary.BigEndian.Uint32(buf.Bytes()))
}

func TestReadRequest_ZeroLength(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRequest(&buf, nil))

	got, err := readRequest(&buf, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, []byte{}, got)
}

func TestWriteResponseError_EmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeResponseError(&buf, ""))
	assert.Equal(t, []byte{0x01}, buf.Bytes())
}
