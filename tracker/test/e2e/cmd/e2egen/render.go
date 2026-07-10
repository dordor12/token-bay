// Package main implements e2egen, a deterministic generator for the
// Docker e2e topology's Ed25519 identities and tracker YAML configs.
//
// e2egen deliberately does not hand-write any YAML: it builds a real
// *config.Config (the exact type tracker/internal/config parses at
// runtime), runs it through config.ApplyDefaults and config.Validate,
// and only then marshals it. That way the generated topology can never
// drift from the tracker's actual config schema — a field rename or a
// new required field in tracker/internal/config breaks this package's
// build or its own self-check, not silently at container startup.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// genOpts are the inputs to generate. Seeds are 32-byte Ed25519 seeds
// (ed25519.SeedSize); every key and config byte is a pure function of
// these fields — no time.Now, no crypto/rand — so the same opts always
// produce byte-identical output.
type genOpts struct {
	OutDir  string
	SeedA   []byte
	SeedB   []byte
	SeedFed []byte
	AddrA   string
	AddrB   string
}

// fedactorFederationAddr is the address tracker-a allowlists for the
// standalone federation-actor test double (a separate task's driver).
// The fedactor dials in; tracker-a never dials this address itself, so
// the value only has to be a well-formed host:port for
// config.Validate's peer-addr check to pass.
const fedactorFederationAddr = "fedactor:7443"

// identity is one node's derived Ed25519 material plus the two hash
// forms other subsystems need: trackerID (sha256 of the raw pubkey,
// used by federation.peers[].tracker_id) and spkiHash (sha256 of the
// DER SubjectPublicKeyInfo encoding — the mTLS pin plugins verify
// against). These are deliberately different encodings of the same
// key; tracker/CLAUDE.md's project memory calls out that they are NOT
// interchangeable.
type identity struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	trackerID [32]byte
	spkiHash  [32]byte
}

func deriveIdentity(seed []byte) (identity, error) {
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return identity{}, fmt.Errorf("derive identity: unexpected public key type %T", priv.Public())
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return identity{}, fmt.Errorf("derive identity: marshal SPKI: %w", err)
	}
	return identity{
		priv:      priv,
		pub:       pub,
		trackerID: sha256.Sum256(pub),
		spkiHash:  sha256.Sum256(spkiDER),
	}, nil
}

func hexPub(pub ed25519.PublicKey) string { return hex.EncodeToString(pub) }

// generate writes the full Docker e2e topology (3 identity keys, 2
// SPKI-hash files, 2 tracker configs) into opts.OutDir.
func generate(opts genOpts) error {
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("generate: mkdir out dir: %w", err)
	}

	idA, err := deriveIdentity(opts.SeedA)
	if err != nil {
		return fmt.Errorf("generate: identity a: %w", err)
	}
	idB, err := deriveIdentity(opts.SeedB)
	if err != nil {
		return fmt.Errorf("generate: identity b: %w", err)
	}
	idFed, err := deriveIdentity(opts.SeedFed)
	if err != nil {
		return fmt.Errorf("generate: identity fed: %w", err)
	}

	if err := writeIdentityKey(opts.OutDir, "a", idA); err != nil {
		return err
	}
	if err := writeIdentityKey(opts.OutDir, "b", idB); err != nil {
		return err
	}
	if err := writeIdentityKey(opts.OutDir, "fed", idFed); err != nil {
		return err
	}

	if err := writeSPKIHash(opts.OutDir, "tracker-a", idA); err != nil {
		return err
	}
	if err := writeSPKIHash(opts.OutDir, "tracker-b", idB); err != nil {
		return err
	}

	if err := writeFedID(opts.OutDir, "tracker-a", idA); err != nil {
		return err
	}
	if err := writeFedID(opts.OutDir, "tracker-b", idB); err != nil {
		return err
	}

	cfgA := buildTrackerConfig("/gen/identity-a.key", []config.FederationPeer{
		{
			TrackerID: hex.EncodeToString(idB.trackerID[:]),
			PubKey:    hexPub(idB.pub),
			Addr:      opts.AddrB + ":7443",
			Region:    "B",
		},
		{
			TrackerID: hex.EncodeToString(idFed.trackerID[:]),
			PubKey:    hexPub(idFed.pub),
			Addr:      fedactorFederationAddr,
			Region:    "FED",
		},
	})
	cfgB := buildTrackerConfig("/gen/identity-b.key", []config.FederationPeer{
		{
			TrackerID: hex.EncodeToString(idA.trackerID[:]),
			PubKey:    hexPub(idA.pub),
			Addr:      opts.AddrA + ":7443",
			Region:    "A",
		},
	})

	scratchDir := filepath.Join(opts.OutDir, ".e2egen-validate-scratch")
	if err := writeTrackerConfig(opts.OutDir, "tracker-a", cfgA, scratchDir); err != nil {
		return err
	}
	if err := writeTrackerConfig(opts.OutDir, "tracker-b", cfgB, scratchDir); err != nil {
		return err
	}

	return nil
}

// buildTrackerConfig returns a fully-populated *config.Config for one
// tracker node. It starts from config.DefaultConfig() — the schema's
// own source of truth for every optional field, including the pricing
// table — and overrides only the node-specific and e2e-tuned fields
// called out in the design brief. Leaving Pricing.Models untouched
// means this generator can never drift from DefaultPriceTable: if that
// table changes, e2egen picks up the change automatically instead of
// carrying a second, staler copy.
func buildTrackerConfig(identityKeyPath string, peers []config.FederationPeer) *config.Config {
	cfg := config.DefaultConfig()

	cfg.DataDir = "/data"
	cfg.LogLevel = "info"

	cfg.Server.ListenAddr = "0.0.0.0:7777"
	cfg.Server.IdentityKeyPath = identityKeyPath
	// The tracker derives its mTLS certificate from the Ed25519 identity
	// key at connection time (see internal/server.CertFromIdentity) — it
	// never opens a cert/key file from disk. config.Validate still
	// requires these two fields to be non-empty (they're part of the
	// "required" §6.1 set), so these are inert placeholders, not real
	// paths anything reads.
	cfg.Server.TLSCertPath = "/gen/unused-tls-cert.pem"
	cfg.Server.TLSKeyPath = "/gen/unused-tls-key.pem"

	cfg.Ledger.StoragePath = "/data/ledger.sqlite"

	// MUST be 0.0.0.0 (not the 127.0.0.1 DefaultConfig value) so the
	// admin API is reachable across the docker-compose network.
	cfg.Admin.ListenAddr = "0.0.0.0:9090"
	cfg.Metrics.ListenAddr = "0.0.0.0:9100"

	cfg.STUNTURN.STUNListenAddr = "0.0.0.0:3478"
	cfg.STUNTURN.TURNListenAddr = "0.0.0.0:3479"

	cfg.Reputation.StoragePath = "/data/reputation.sqlite"
	cfg.Reputation.EvaluationIntervalS = 1
	cfg.Reputation.MinPopulationForZScore = 3
	cfg.Reputation.ZScoreThreshold = 2.5

	cfg.Admission.TLogPath = "/data/admission.tlog"
	cfg.Admission.SnapshotPathPrefix = "/data/admission.snapshot"

	// Settlement timings tuned tight for fast e2e runs. config.Validate
	// §6.5 requires tunnel_setup_ms < settlement_timeout_s*1000 and
	// reservation_ttl_s >= settlement_timeout_s; 500 < 3*1000 and 5 >= 3
	// satisfy both with room to spare.
	cfg.Settlement.SettlementTimeoutS = 3
	cfg.Settlement.TunnelSetupMs = 500
	cfg.Settlement.ReservationTTLS = 5

	// DefaultConfig's broker.offer_timeout_ms (1500) is already suitable
	// for e2e; set explicitly so a future DefaultConfig change can't
	// silently slow this topology down.
	cfg.Broker.OfferTimeoutMs = 1500

	cfg.Federation.ListenAddr = "0.0.0.0:7443"
	// The design brief asks for a 2s publish cadence for fast e2e
	// gossip, but config.Validate §6.6 clamps publish_cadence_s to
	// [60, 86400]. 60 is the fastest value the validator accepts.
	cfg.Federation.PublishCadenceS = 60
	cfg.Federation.Peers = peers

	return cfg
}

// writeIdentityKey writes name's raw 64-byte Ed25519 private key to
// identity-<name>.key, mode 0644 so a bind-mounted file (owned by the
// host user who ran e2egen) is still readable by the container's uid
// 1000 (see deployments/docker/Dockerfile).
func writeIdentityKey(outDir, name string, id identity) error {
	path := filepath.Join(outDir, "identity-"+name+".key")
	if err := os.WriteFile(path, id.priv, 0o644); err != nil { //nolint:gosec // deliberately world-readable, see comment above
		return fmt.Errorf("generate: write %s: %w", path, err)
	}
	return nil
}

// writeSPKIHash writes the hex-encoded SHA-256 of trackerName's
// DER-encoded SubjectPublicKeyInfo to <trackerName>.spki — the mTLS pin
// plugin actors need (shared/... TrackerEndpoint.IdentityHash). No
// trailing newline: the file is exactly the hex digest.
func writeSPKIHash(outDir, trackerName string, id identity) error {
	path := filepath.Join(outDir, trackerName+".spki")
	hexHash := hex.EncodeToString(id.spkiHash[:])
	if err := os.WriteFile(path, []byte(hexHash), 0o644); err != nil { //nolint:gosec // world-readable like identity keys: bind-mounted into containers running as a non-owner uid
		return fmt.Errorf("generate: write %s: %w", path, err)
	}
	return nil
}

// writeFedID writes the hex-encoded SHA-256 of trackerName's raw Ed25519
// public key to <trackerName>.fedid — the FEDERATION tracker_id (the
// identity.trackerID field also used in federation.peers[].tracker_id),
// which the consumer actor's /transfer needs to identify a tracker's
// region. This is a distinct encoding from the mTLS SPKI hash written by
// writeSPKIHash: sha256(raw pubkey) here vs. sha256(DER SPKI) there. No
// trailing newline: the file is exactly the hex digest.
func writeFedID(outDir, trackerName string, id identity) error {
	path := filepath.Join(outDir, trackerName+".fedid")
	hexID := hex.EncodeToString(id.trackerID[:])
	if err := os.WriteFile(path, []byte(hexID), 0o644); err != nil { //nolint:gosec // world-readable like identity keys: bind-mounted into containers running as a non-owner uid
		return fmt.Errorf("generate: write %s: %w", path, err)
	}
	return nil
}

// writeTrackerConfig applies defaults, self-validates, and marshals cfg
// to <name>.yaml.
//
// The self-validation runs against a shallow copy with
// admission.tlog_path repointed at a real, on-host scratch directory
// rather than the literal "/data" the YAML actually carries. "/data"
// only exists inside the tracker container image (deployments/docker/
// Dockerfile mkdir's and chowns it at build time) — it does not, and
// per the design brief must not, exist on the host machine that runs
// e2egen or its tests. Swapping in a real directory for the fs-existence
// probe lets config.Validate's every OTHER invariant run for real
// (settlement co-constraints, federation ranges, listener collisions,
// pricing, ...) without requiring root or a container to do it.
func writeTrackerConfig(outDir, name string, cfg *config.Config, scratchDir string) error {
	config.ApplyDefaults(cfg)

	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return fmt.Errorf("generate: mkdir validate scratch dir: %w", err)
	}
	validateCopy := *cfg
	validateCopy.Admission.TLogPath = filepath.Join(scratchDir, name+"-admission.tlog")
	if err := config.Validate(&validateCopy); err != nil {
		return fmt.Errorf("generate: %s config failed validation: %w", name, err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("generate: marshal %s config: %w", name, err)
	}
	path := filepath.Join(outDir, name+".yaml")
	if err := os.WriteFile(path, out, 0o644); err != nil { //nolint:gosec // world-readable: the tracker container reads this bind-mounted config as uid 1000
		return fmt.Errorf("generate: write %s: %w", path, err)
	}
	return nil
}
