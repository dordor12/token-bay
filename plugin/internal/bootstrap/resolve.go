package bootstrap

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// BootstrapSignerPubkeyHex is the hex-encoded Ed25519 public key the
// build was anchored to for verifying signed bootstrap lists. Set via
// `-ldflags '-X
// github.com/token-bay/token-bay/plugin/internal/bootstrap.BootstrapSignerPubkeyHex=<hex>'`
// at release time. Empty in dev/test builds — callers must supply a
// pubkey via AutoResolveConfig.SignerPubkey directly.
//
//nolint:gochecknoglobals // build-time injection point
var BootstrapSignerPubkeyHex = ""

// BuiltinBootstrapSignedHex is a hex-encoded marshaled BootstrapPeerList
// compiled into the release binary. When the runtime cannot find
// $TOKEN_BAY_HOME/bootstrap.signed, this falls back. Empty in dev/test
// builds — those rely on a real bootstrap.signed file or pre-populated
// AutoResolveConfig.BuiltinSigned.
//
//nolint:gochecknoglobals // build-time injection point
var BuiltinBootstrapSignedHex = ""

// BuildTimeSignerPubkey returns the Ed25519 pubkey set at build time,
// or an error if none is configured. Production builds populate
// BootstrapSignerPubkeyHex via -ldflags.
func BuildTimeSignerPubkey() (ed25519.PublicKey, error) {
	if BootstrapSignerPubkeyHex == "" {
		return nil, errors.New("bootstrap: BootstrapSignerPubkeyHex not configured at build time")
	}
	raw, err := hex.DecodeString(BootstrapSignerPubkeyHex)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: BootstrapSignerPubkeyHex invalid hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bootstrap: BootstrapSignerPubkeyHex len %d != %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// BuiltinBootstrapSigned returns the compiled-in fallback bootstrap
// list bytes (a marshaled BootstrapPeerList), or an empty slice if
// none was injected at build time.
func BuiltinBootstrapSigned() []byte {
	if BuiltinBootstrapSignedHex == "" {
		return nil
	}
	raw, err := hex.DecodeString(BuiltinBootstrapSignedHex)
	if err != nil {
		return nil
	}
	return raw
}

// AutoResolveConfig parameterizes the `tracker = "auto"` bootstrap
// resolution. CfgDir is $TOKEN_BAY_HOME; SignerPubkey verifies the
// signed list; BuiltinSigned is the optional fallback bytes; Now is the
// expiry-check anchor; MaxEndpoints caps the returned slice (≤ 0 = all).
type AutoResolveConfig struct {
	CfgDir        string
	SignerPubkey  ed25519.PublicKey
	BuiltinSigned []byte
	Now           time.Time
	MaxEndpoints  int
}

// ResolveAutoEndpoints implements §7.2 plugin auto-bootstrap. Tries
// $CfgDir/bootstrap.signed first; on os.ErrNotExist falls back to
// BuiltinSigned bytes; if neither is available, returns an error.
//
// Any error from ParseSignedBootstrapList on the file path (sig
// invalid, expired, malformed) is surfaced as-is — silently
// falling back to a possibly-stale built-in would mask tampering.
func ResolveAutoEndpoints(cfg AutoResolveConfig) ([]trackerclient.TrackerEndpoint, error) {
	if cfg.CfgDir == "" {
		return nil, errors.New("bootstrap: CfgDir required")
	}
	path := filepath.Join(cfg.CfgDir, "bootstrap.signed")
	endpoints, err := ParseSignedBootstrapList(path, cfg.SignerPubkey, cfg.Now)
	if err == nil {
		return capEndpoints(endpoints, cfg.MaxEndpoints), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("bootstrap: %s: %w", path, err)
	}
	if len(cfg.BuiltinSigned) == 0 {
		return nil, fmt.Errorf("bootstrap: no %s and no built-in fallback configured", path)
	}
	endpoints, err = parseSignedBootstrapBytes(cfg.BuiltinSigned, cfg.SignerPubkey, cfg.Now)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: built-in fallback invalid: %w", err)
	}
	return capEndpoints(endpoints, cfg.MaxEndpoints), nil
}

// parseSignedBootstrapBytes is the in-memory twin of
// ParseSignedBootstrapList; both share validation + verification rules.
func parseSignedBootstrapBytes(raw []byte, signerPub ed25519.PublicKey, now time.Time) ([]trackerclient.TrackerEndpoint, error) {
	if len(signerPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bootstrap: signer pubkey len %d != %d", len(signerPub), ed25519.PublicKeySize)
	}
	var list tbproto.BootstrapPeerList
	if err := proto.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("bootstrap: unmarshal: %w", err)
	}
	if err := tbproto.ValidateBootstrapPeerList(&list); err != nil {
		return nil, fmt.Errorf("bootstrap: validate: %w", err)
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

func capEndpoints(eps []trackerclient.TrackerEndpoint, n int) []trackerclient.TrackerEndpoint {
	if n <= 0 || len(eps) <= n {
		return eps
	}
	return eps[:n]
}
