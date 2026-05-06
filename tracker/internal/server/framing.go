package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/signing"
)

// Wire-frame layout (verbatim from plugin trackerclient §5.1 / spec §5.3):
//
//	+---------+--------------------------------+
//	| len:u32 | proto bytes (DeterministicMar) |
//	+---------+--------------------------------+
//
// len is big-endian. Frames whose declared length exceeds maxFrameSize
// are rejected with ErrFrameTooLarge.

// ErrFrameTooLarge is returned when a peer sends — or we attempt to
// send — a frame whose declared length exceeds maxFrameSize.
var ErrFrameTooLarge = errors.New("server: frame exceeds max size")

// WriteFrame serializes m via signing.DeterministicMarshal and writes a
// single length-prefixed frame to w.
func WriteFrame(w io.Writer, m proto.Message, maxFrameSize int) error {
	buf, err := signing.DeterministicMarshal(m)
	if err != nil {
		return fmt.Errorf("server: marshal: %w", err)
	}
	if len(buf) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(buf), maxFrameSize)
	}
	var hdr [4]byte
	//nolint:gosec // G115: len(buf) bounded by maxFrameSize check, ≤ 1 MiB in practice
	binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("server: write header: %w", err)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("server: write body: %w", err)
	}
	return nil
}

// ReadFrame reads a single length-prefixed frame from r and unmarshals
// it into dst. Returns ErrFrameTooLarge if the declared length exceeds
// maxFrameSize, io.EOF if r is at end-of-stream before a header arrives,
// io.ErrUnexpectedEOF on a short body.
//
// A fresh body buffer is allocated per frame; bounded by maxFrameSize.
func ReadFrame(r io.Reader, dst proto.Message, maxFrameSize int) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err // io.EOF / io.ErrUnexpectedEOF surfaced verbatim
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int(n) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, maxFrameSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("server: read body: %w", err)
	}
	if err := proto.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("server: unmarshal: %w", err)
	}
	return nil
}
