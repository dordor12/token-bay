package bootstrap

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// signedListSkewToleranceS mirrors plugin/internal/trackerclient's
// bootstrapPeerListSkewToleranceS — the file-borne form should accept the
// same clock-skew window as the live RPC form.
const signedListSkewToleranceS = 60

// ErrBootstrapListExpired is returned when a signed bootstrap list's
// ExpiresAt is more than the skew tolerance in the past.
var ErrBootstrapListExpired = errors.New("bootstrap: signed bootstrap list expired")

// ParseSignedBootstrapList reads, validates, signature-verifies, and
// expiry-checks a marshaled BootstrapPeerList file, then converts it to
// the trackerclient endpoint shape used to dial the QUIC mTLS handshake.
//
// signerPub is the Ed25519 public key the file was signed with —
// callers typically inject this at build time (release-key constant)
// or load it from cfg.
func ParseSignedBootstrapList(path string, signerPub ed25519.PublicKey, now time.Time) ([]trackerclient.TrackerEndpoint, error) {
	if len(signerPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bootstrap: signer pubkey len %d != %d", len(signerPub), ed25519.PublicKeySize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list tbproto.BootstrapPeerList
	if err := proto.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("bootstrap: unmarshal %s: %w", path, err)
	}
	if err := tbproto.ValidateBootstrapPeerList(&list); err != nil {
		return nil, fmt.Errorf("bootstrap: validate %s: %w", path, err)
	}
	if err := tbproto.VerifyBootstrapPeerListSig(signerPub, &list); err != nil {
		return nil, err
	}
	if uint64(now.Unix()) > list.ExpiresAt+signedListSkewToleranceS { //nolint:gosec
		return nil, ErrBootstrapListExpired
	}

	endpoints := make([]trackerclient.TrackerEndpoint, 0, len(list.Peers))
	for _, p := range list.Peers {
		var hash [32]byte
		copy(hash[:], p.TrackerId)
		endpoints = append(endpoints, trackerclient.TrackerEndpoint{
			Addr:         p.Addr,
			IdentityHash: hash,
			Region:       p.RegionHint,
		})
	}
	return endpoints, nil
}
