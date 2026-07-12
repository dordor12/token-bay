//go:build perf

package perf

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

// trackerNode is one pre-rendered fleet member: deterministic identity
// (seed = sha256("token-bay-perf-tracker-<i>")), the two hash forms
// clients and federation need, and its rendered YAML on disk. All
// TrackersMax nodes are rendered up-front with FULL-MESH federation
// allowlists (spec §4): the validator requires static peer lists, and
// dialing a not-yet-started peer just redials with backoff.
type trackerNode struct {
	Index    int
	Name     string // container network alias, e.g. perf-tracker-0
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	SPKIHash [32]byte // mTLS pin: sha256(DER SubjectPublicKeyInfo)
	FedID    [32]byte // federation tracker_id: sha256(raw pubkey)
}

func (n *trackerNode) FedIDHex() string { return hex.EncodeToString(n.FedID[:]) }

// renderFleet derives count tracker identities and writes
// identity-<i>.key + tracker-<i>.yaml into genDir.
func renderFleet(genDir string, count int) ([]*trackerNode, error) {
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return nil, fmt.Errorf("perf: mkdir gen dir: %w", err)
	}
	nodes := make([]*trackerNode, 0, count)
	for i := 0; i < count; i++ {
		seed := sha256.Sum256(fmt.Appendf(nil, "token-bay-perf-tracker-%d", i))
		priv := ed25519.NewKeyFromSeed(seed[:])
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("perf: unexpected public key type %T", priv.Public())
		}
		spkiDER, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, fmt.Errorf("perf: marshal SPKI for tracker %d: %w", i, err)
		}
		nodes = append(nodes, &trackerNode{
			Index:    i,
			Name:     fmt.Sprintf("perf-tracker-%d", i),
			priv:     priv,
			pub:      pub,
			SPKIHash: sha256.Sum256(spkiDER),
			FedID:    sha256.Sum256(pub),
		})
	}

	scratch := filepath.Join(genDir, ".perf-validate-scratch")
	for _, n := range nodes {
		keyPath := filepath.Join(genDir, fmt.Sprintf("identity-%d.key", n.Index))
		//nolint:gosec // world-readable: bind-mounted into containers running uid 1000
		if err := os.WriteFile(keyPath, n.priv, 0o644); err != nil {
			return nil, fmt.Errorf("perf: write %s: %w", keyPath, err)
		}
		cfg := buildPerfTrackerConfig(n, nodes)
		if err := writeTrackerYAML(genDir, n, cfg, scratch); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// buildPerfTrackerConfig mirrors e2egen's approach (start from
// config.DefaultConfig so the pricing table can never drift) with
// perf-tuned timings: settlement windows loose enough for load,
// publish cadence at the validator's 60s floor so an hour of soak
// carries real gossip.
func buildPerfTrackerConfig(n *trackerNode, all []*trackerNode) *config.Config {
	cfg := config.DefaultConfig()

	cfg.DataDir = "/data"
	cfg.LogLevel = "warn" // 10k clients at info would drown the container logs

	cfg.Server.ListenAddr = "0.0.0.0:7777"
	cfg.Server.IdentityKeyPath = fmt.Sprintf("/gen/identity-%d.key", n.Index)
	// Inert placeholders — the tracker derives its mTLS cert from the
	// identity key (server.CertFromIdentity); config.Validate just
	// requires the fields to be non-empty (same trick as e2egen).
	cfg.Server.TLSCertPath = "/gen/unused-tls-cert.pem"
	cfg.Server.TLSKeyPath = "/gen/unused-tls-key.pem"

	cfg.Ledger.StoragePath = "/data/ledger.sqlite"
	cfg.Admin.ListenAddr = "0.0.0.0:9090"
	cfg.Metrics.ListenAddr = "0.0.0.0:9100"
	cfg.STUNTURN.STUNListenAddr = "0.0.0.0:3478"
	cfg.STUNTURN.TURNListenAddr = "0.0.0.0:3479"
	cfg.Reputation.StoragePath = "/data/reputation.sqlite"
	cfg.Admission.TLogPath = "/data/admission.tlog"
	cfg.Admission.SnapshotPathPrefix = "/data/admission.snapshot"

	// Loose enough that a loaded tracker doesn't time out its own
	// settlements, tight enough that abandoned reservations recycle
	// within the run (validator: tunnel_setup_ms < settlement_timeout_s
	// * 1000, reservation_ttl_s >= settlement_timeout_s).
	cfg.Settlement.SettlementTimeoutS = 10
	cfg.Settlement.TunnelSetupMs = 500
	cfg.Settlement.ReservationTTLS = 15
	cfg.Broker.OfferTimeoutMs = 2000

	cfg.Federation.ListenAddr = "0.0.0.0:7443"
	cfg.Federation.PublishCadenceS = 60 // validator floor
	peers := make([]config.FederationPeer, 0, len(all)-1)
	for _, p := range all {
		if p.Index == n.Index {
			continue
		}
		peers = append(peers, config.FederationPeer{
			TrackerID: p.FedIDHex(),
			PubKey:    hex.EncodeToString(p.pub),
			Addr:      p.Name + ":7443",
			Region:    fmt.Sprintf("PERF%d", p.Index),
		})
	}
	cfg.Federation.Peers = peers

	return cfg
}

// writeTrackerYAML applies defaults, self-validates against a host
// scratch dir (the literal /data only exists inside the container
// image — same dodge as e2egen), and writes tracker-<i>.yaml.
func writeTrackerYAML(genDir string, n *trackerNode, cfg *config.Config, scratch string) error {
	config.ApplyDefaults(cfg)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return fmt.Errorf("perf: mkdir validate scratch: %w", err)
	}
	validateCopy := *cfg
	validateCopy.Admission.TLogPath = filepath.Join(scratch, fmt.Sprintf("tracker-%d-admission.tlog", n.Index))
	if err := config.Validate(&validateCopy); err != nil {
		return fmt.Errorf("perf: tracker %d config failed validation: %w", n.Index, err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("perf: marshal tracker %d config: %w", n.Index, err)
	}
	path := filepath.Join(genDir, fmt.Sprintf("tracker-%d.yaml", n.Index))
	//nolint:gosec // world-readable: read inside the container as uid 1000
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("perf: write %s: %w", path, err)
	}
	return nil
}
