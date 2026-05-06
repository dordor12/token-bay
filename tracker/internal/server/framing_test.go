package server_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

func TestFraming_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: []byte{1, 2, 3},
	}
	if err := server.WriteFrame(&buf, in, 1<<20); err != nil {
		t.Fatal(err)
	}
	var got tbproto.RpcRequest
	if err := server.ReadFrame(&buf, &got, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got.Method != in.Method || !bytes.Equal(got.Payload, in.Payload) {
		t.Fatalf("got %+v", &got)
	}
}

func TestFraming_OversizeOnWrite(t *testing.T) {
	var buf bytes.Buffer
	in := &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: bytes.Repeat([]byte{0x55}, 1<<20),
	}
	err := server.WriteFrame(&buf, in, 1024)
	if !errors.Is(err, server.ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestFraming_OversizeOnRead(t *testing.T) {
	hdr := []byte{0x00, 0x00, 0x10, 0x00} // claim 4096 bytes
	r := bytes.NewReader(hdr)
	var dst tbproto.RpcRequest
	err := server.ReadFrame(r, &dst, 1024)
	if !errors.Is(err, server.ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestFraming_TruncatedHeader(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x01}) // only 2 of 4 bytes
	var dst tbproto.RpcRequest
	err := server.ReadFrame(r, &dst, 1024)
	if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
}

func TestFraming_TruncatedBody(t *testing.T) {
	hdr := []byte{0x00, 0x00, 0x00, 0x10} // claim 16 bytes
	r := bytes.NewReader(append(hdr, 0x01, 0x02, 0x03, 0x04))
	var dst tbproto.RpcRequest
	err := server.ReadFrame(r, &dst, 1024)
	if err == nil {
		t.Fatal("want error on truncated body")
	}
}

func TestFraming_EOFOnEmptyReader(t *testing.T) {
	r := bytes.NewReader(nil)
	var dst tbproto.RpcRequest
	err := server.ReadFrame(r, &dst, 1024)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
}

func TestFraming_GarbageBody_UnmarshalError(t *testing.T) {
	// 4-byte header claiming 5 bytes + 5 bytes that aren't a valid proto.
	r := bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x05, 0xff, 0xff, 0xff, 0xff, 0xff})
	var dst tbproto.RpcRequest
	err := server.ReadFrame(r, &dst, 1024)
	if err == nil {
		t.Fatal("want unmarshal error")
	}
}

func TestFraming_ZeroLenFrame(t *testing.T) {
	// 4-byte header claiming 0 bytes + nothing → unmarshal succeeds
	// (empty proto), Read returns nil. Useful invariant for the server
	// dispatcher's bad-payload check.
	r := bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x00})
	var dst tbproto.RpcRequest
	if err := server.ReadFrame(r, &dst, 1024); err != nil {
		t.Fatalf("zero-len frame: %v", err)
	}
}
