// Package wire provides the length-prefixed deterministic-proto codec
// used on every Token-Bay client↔tracker stream.
//
// Wire format per frame:
//
//	+---------+--------------------------------+
//	| len:u32 | proto bytes (DeterministicMar) |
//	+---------+--------------------------------+
//
// len is big-endian and bounds the payload length. Frames whose declared
// length exceeds maxFrameSize are rejected with ErrFrameTooLarge.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/signing"
)

// ErrFrameTooLarge is returned when a peer sends or we attempt to send a
// frame whose declared length exceeds maxFrameSize.
var ErrFrameTooLarge = errors.New("wire: frame exceeds max size")

// ReadWriter is the io.Reader and io.Writer halves of a stream.
type ReadWriter interface {
	io.Reader
	io.Writer
}

// Write serializes m using DeterministicMarshal and writes a single
// length-prefixed frame to w. Returns ErrFrameTooLarge if the encoded
// payload would exceed maxFrameSize.
func Write(w io.Writer, m proto.Message, maxFrameSize int) error {
	buf, err := signing.DeterministicMarshal(m)
	if err != nil {
		return fmt.Errorf("wire: marshal: %w", err)
	}
	if len(buf) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(buf), maxFrameSize)
	}
	var hdr [4]byte
	//nolint:gosec // G115: len(buf) is bounded above by maxFrameSize check; in practice <= 1 MiB
	binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("wire: write header: %w", err)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("wire: write body: %w", err)
	}
	return nil
}

// Read reads a single length-prefixed frame from r and unmarshals it
// into dst. Returns ErrFrameTooLarge if the declared length exceeds
// maxFrameSize, io.EOF if r is at end-of-stream before a header arrives,
// and io.ErrUnexpectedEOF if the body is short.
//
// A fresh body buffer is allocated per frame; bounded by maxFrameSize.
func Read(r io.Reader, dst proto.Message, maxFrameSize int) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err // io.EOF or io.ErrUnexpectedEOF surfaced verbatim
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int(n) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, maxFrameSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("wire: read body: %w", err)
	}
	if err := proto.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("wire: unmarshal: %w", err)
	}
	return nil
}
