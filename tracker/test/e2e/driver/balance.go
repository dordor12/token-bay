//go:build e2e

package driver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/server"
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
// TLS verify callback below).
//
// Returns the verified snapshot on success. Callers read
// snap.GetBody().GetCredits() for the balance; see BalanceCredits for a
// nil-safe convenience accessor.
func BalanceOf(trackerAddr string, trackerSPKIHash [32]byte, identityID [32]byte) (*tbproto.SignedBalanceSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("driver: generate client identity: %w", err)
	}
	cliCert, err := server.CertFromIdentity(priv)
	if err != nil {
		return nil, fmt.Errorf("driver: build client cert: %w", err)
	}

	var trackerPub ed25519.PublicKey
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cliCert},
		//nolint:gosec // G402: InsecureSkipVerify is required so quic-go
		// invokes VerifyPeerCertificate instead of doing web-PKI chain
		// validation; the SPKI pin below is the real identity check
		// (same pattern as tracker/test/integration's fixture.dial).
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("driver: tracker presented no certificate")
			}
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("driver: parse tracker cert: %w", err)
			}
			got := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
			if got != trackerSPKIHash {
				return fmt.Errorf("driver: tracker SPKI mismatch: got %x want %x", got, trackerSPKIHash)
			}
			pub, ok := parsed.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("driver: tracker cert is not Ed25519")
			}
			trackerPub = pub
			return nil
		},
		NextProtos:             []string{balanceALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}

	conn, err := quicgo.DialAddr(ctx, trackerAddr, tlsCfg, &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	})
	if err != nil {
		return nil, fmt.Errorf("driver: quic dial %s: %w", trackerAddr, err)
	}
	defer func() { _ = conn.CloseWithError(0, "driver: balance rpc done") }()

	// tracker/internal/server's serveConn loop blocks its first
	// AcceptStream on a dedicated heartbeat stream before entering the
	// per-connection RPC loop (see tracker/test/integration/rpc_path_test.go
	// openHB) — open and hold one open for the lifetime of the RPC.
	hb, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("driver: open heartbeat stream: %w", err)
	}
	defer hb.Close()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("driver: open rpc stream: %w", err)
	}
	defer stream.Close()

	payload, err := proto.Marshal(&tbproto.BalanceRequest{IdentityId: identityID[:]})
	if err != nil {
		return nil, fmt.Errorf("driver: marshal BalanceRequest: %w", err)
	}
	req := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BALANCE, Payload: payload}
	if err := writeFrame(stream, req, maxBalanceFrameSize); err != nil {
		return nil, fmt.Errorf("driver: write BALANCE request: %w", err)
	}

	var resp tbproto.RpcResponse
	if err := readFrame(stream, &resp, maxBalanceFrameSize); err != nil {
		return nil, fmt.Errorf("driver: read BALANCE response: %w", err)
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		return nil, fmt.Errorf("driver: BALANCE rpc status=%s error=%v", resp.Status, resp.GetError())
	}

	var snap tbproto.SignedBalanceSnapshot
	if err := proto.Unmarshal(resp.Payload, &snap); err != nil {
		return nil, fmt.Errorf("driver: unmarshal SignedBalanceSnapshot: %w", err)
	}
	if trackerPub == nil {
		return nil, errors.New("driver: tracker pubkey not captured during handshake")
	}
	if !signing.VerifyBalanceSnapshot(trackerPub, &snap) {
		return nil, errors.New("driver: SignedBalanceSnapshot failed tracker signature verification")
	}
	return &snap, nil
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
