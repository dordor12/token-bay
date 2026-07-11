//go:build e2e

package driver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// balanceALPN is the Token-Bay tracker<->plugin QUIC ALPN. Mirrors
// tracker/internal/server.ALPN ("tokenbay/1") and plugin/internal/idtls's
// copy of the same constant — every leg of the protocol must agree.
const balanceALPN = "tokenbay/1"

// maxBalanceFrameSize bounds a single length-prefixed RPC frame on the
// wire below. Mirrors the 1 MiB default used by
// plugin/internal/trackerclient/internal/wire and
// tracker/internal/server.WriteFrame/ReadFrame call sites.
const maxBalanceFrameSize = 1 << 20

// dialTimeout bounds the whole BalanceOf round trip: QUIC handshake +
// heartbeat-stream open + RPC stream open/write/read.
const dialTimeout = 10 * time.Second

// BalanceOf dials the tracker at trackerAddr (host:port, UDP — QUIC) over
// a freshly generated Ed25519 client identity, pins the presented server
// certificate by trackerSPKIHash (sha256 of the tracker's DER
// SubjectPublicKeyInfo — see tracker/internal/server.SPKIToIdentityID,
// the same value e2egen writes to tracker-{a,b}.spki per the plan's Task
// 16 note), issues a single RPC_METHOD_BALANCE request for identityID,
// and verifies the returned SignedBalanceSnapshot's tracker signature
// against the tracker's own pubkey (recovered from the cert it presented
// during the handshake, which is already bound to trackerSPKIHash by the
// TLS verify callback).
//
// One-shot convenience over the reusable RPCClient (rpc.go), which owns
// the dial/pin/heartbeat/framing machinery.
//
// Returns the verified snapshot on success. Callers read
// snap.GetBody().GetCredits() for the balance; see BalanceCredits for a
// nil-safe convenience accessor.
func BalanceOf(trackerAddr string, trackerSPKIHash [32]byte, identityID [32]byte) (*tbproto.SignedBalanceSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	cli, err := DialRPC(ctx, trackerAddr, trackerSPKIHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()
	return cli.VerifiedBalance(ctx, identityID)
}

// BalanceCredits is a nil-safe convenience accessor for the snapshot's
// credit balance, mirroring the proto's own GetBody().GetCredits() chain
// but explicit about the driver's contract (BalanceOf never returns a
// snapshot with a nil Body — this exists so scenario tests read cleanly).
func BalanceCredits(snap *tbproto.SignedBalanceSnapshot) int64 {
	return snap.GetBody().GetCredits()
}

// --- local frame codec --------------------------------------------------
//
// Reimplementation of the tiny length-prefixed frame codec used on every
// Token-Bay client<->tracker stream (see
// plugin/internal/trackerclient/internal/wire and
// tracker/internal/server/framing.go for the two existing copies). The
// driver package lives under the tracker module's test tree but cannot
// import plugin/internal/trackerclient/internal/wire — that path is
// gated by Go's internal-package visibility rule to the plugin module
// tree. Reimplemented locally rather than depending on
// tracker/internal/server's copy so this codec stays stable even as the
// production server package evolves.
//
// Wire format per frame (big-endian u32 length prefix + DeterministicMarshal
// proto bytes):
//
//	+---------+--------------------------------+
//	| len:u32 | proto bytes (DeterministicMar) |
//	+---------+--------------------------------+

// errFrameTooLarge is returned when a frame's encoded/declared length
// exceeds the caller-supplied max.
var errFrameTooLarge = errors.New("driver: frame exceeds max size")

// writeFrame serializes m via signing.DeterministicMarshal (the single
// canonical-bytes choke point, shared/CLAUDE.md rule 6) and writes one
// length-prefixed frame to w.
func writeFrame(w io.Writer, m proto.Message, maxFrameSize int) error {
	buf, err := signing.DeterministicMarshal(m)
	if err != nil {
		return fmt.Errorf("driver: marshal: %w", err)
	}
	if len(buf) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", errFrameTooLarge, len(buf), maxFrameSize)
	}
	var hdr [4]byte
	//nolint:gosec // G115: len(buf) bounded above by maxFrameSize check
	binary.BigEndian.PutUint32(hdr[:], uint32(len(buf)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("driver: write header: %w", err)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("driver: write body: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed frame from r and unmarshals it
// into dst.
func readFrame(r io.Reader, dst proto.Message, maxFrameSize int) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err // io.EOF / io.ErrUnexpectedEOF surfaced verbatim
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int(n) > maxFrameSize {
		return fmt.Errorf("%w: %d > %d", errFrameTooLarge, n, maxFrameSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("driver: read body: %w", err)
	}
	if err := proto.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("driver: unmarshal: %w", err)
	}
	return nil
}
