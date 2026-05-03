package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// status enum on the seeder→consumer half of the bidi stream.
type status byte

const (
	statusOK    status = 0x00
	statusError status = 0x01
)

// requestHeaderLen is the wire-format length-prefix size in bytes.
const requestHeaderLen = 4

// maxErrorBytes caps the length of the seeder→consumer error message
// payload (in bytes). Both writer and reader enforce it, matching
// the doc.go promise of "UTF-8 error message (≤ 4 KiB)".
const maxErrorBytes = 4 << 10

// writeRequest writes [4 BE length] [body] to w.
func writeRequest(w io.Writer, body []byte) error {
	var hdr [requestHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body))) //nolint:gosec // len bounded by caller
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// readRequest reads the length prefix and body. Returns ErrRequestTooLarge
// when the prefix exceeds maxBytes (the body is NOT drained); returns
// ErrFramingViolation on short header or truncated body.
func readRequest(r io.Reader, maxBytes int) ([]byte, error) {
	var hdr [requestHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrFramingViolation, err)
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if maxBytes > 0 && n > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrRequestTooLarge, n, maxBytes)
	}
	if n == 0 {
		return []byte{}, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: body: %v", ErrFramingViolation, err)
	}
	return body, nil
}

// writeResponseStatus writes a single status byte (no payload).
func writeResponseStatus(w io.Writer, s status) error {
	_, err := w.Write([]byte{byte(s)})
	return err
}

// writeResponseError writes [0x01] followed by msg's UTF-8 bytes,
// truncated to maxErrorBytes. The caller is expected to close the
// stream after writing.
func writeResponseError(w io.Writer, msg string) error {
	if _, err := w.Write([]byte{byte(statusError)}); err != nil {
		return err
	}
	if msg == "" {
		return nil
	}
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes]
	}
	_, err := w.Write([]byte(msg))
	return err
}

// readErrorMessage drains up to maxErrorBytes from r and returns
// the bytes. Used by consumer-side Receive after readResponseStatus
// reports statusError. EOF is not an error — the peer closing write
// signals end-of-message.
func readErrorMessage(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, int64(maxErrorBytes))
	return io.ReadAll(limited)
}

// readResponseStatus consumes the first byte of the seeder→consumer
// half-stream and returns the typed status. Subsequent bytes are the
// caller's to consume.
func readResponseStatus(r io.Reader) (status, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("%w: empty response stream", ErrFramingViolation)
		}
		return 0, fmt.Errorf("%w: read status: %v", ErrFramingViolation, err)
	}
	switch status(b[0]) {
	case statusOK, statusError:
		return status(b[0]), nil
	default:
		return 0, fmt.Errorf("%w: unknown status byte 0x%02x", ErrFramingViolation, b[0])
	}
}
