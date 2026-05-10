package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/config"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

const (
	enrollmentFilename      = "enrollment.json"
	enrollmentSchemaVersion = 1
	connectTimeout          = 10 * time.Second
)

// EnrollClient is the runEnroll seam for the tracker. *trackerclient.Client
// satisfies it; tests inject a fake.
type EnrollClient interface {
	Enroll(ctx context.Context, r *trackerclient.EnrollRequest) (*trackerclient.EnrollResponse, error)
}

// enrollOptions carries the inputs runEnroll needs. Production wiring is in
// newEnrollCmd; tests construct this directly with stub seams.
type enrollOptions struct {
	ConfigDir    string
	AuditLogPath string
	Tracker      string
	TrackerHash  [32]byte
	Region       string
	Role         uint32
	ReEnroll     bool

	AuthProber ccproxy.AuthProber
	// NewClient takes the freshly generated signer (used as the QUIC TLS
	// identity) and returns a connected EnrollClient plus a teardown.
	// Tests return a fake; production constructs trackerclient.Client.
	NewClient func(ctx context.Context, s *identity.Signer) (EnrollClient, func(), error)
	Now       func() time.Time
	Stderr    io.Writer
}

// runEnroll performs the full first-run identity-binding flow per plugin
// spec §4. Returns a wrapped error on any step; on success, the keypair,
// identity record, enrollment sidecar, and starter-grant audit-log line are
// all on disk under opts.ConfigDir / opts.AuditLogPath.
func runEnroll(ctx context.Context, opts enrollOptions) error {
	if err := validateEnrollOptions(opts); err != nil {
		return err
	}

	state, err := opts.AuthProber.Probe(ctx)
	if err != nil {
		return fmt.Errorf("enroll: probe `claude auth status --json` (is the claude binary on PATH?): %w", err)
	}
	if ok, reason := state.IsCompatible(); !ok {
		return fmt.Errorf("enroll refused: %s", reason)
	}

	fingerprint, err := identity.AccountFingerprint(identity.AuthSnapshot{
		LoggedIn:    state.LoggedIn,
		AuthMethod:  state.AuthMethod,
		APIProvider: state.APIProvider,
		OrgID:       state.OrgID,
	})
	if err != nil {
		return fmt.Errorf("enroll: derive account fingerprint: %w", err)
	}

	keyPath := filepath.Join(opts.ConfigDir, "identity.key")
	if err := preflightKeyPath(keyPath, opts.ReEnroll); err != nil {
		return err
	}

	signer, err := identity.Generate()
	if err != nil {
		return fmt.Errorf("enroll: generate keypair: %w", err)
	}

	payload, err := identity.BuildEnrollPayload(signer, fingerprint, opts.Role)
	if err != nil {
		return fmt.Errorf("enroll: build payload: %w", err)
	}

	client, closeClient, err := opts.NewClient(ctx, signer)
	if err != nil {
		return fmt.Errorf("enroll: open tracker connection: %w", err)
	}
	defer closeClient()

	resp, err := client.Enroll(ctx, &trackerclient.EnrollRequest{
		IdentityPubkey:     payload.IdentityPubkey,
		Role:               payload.Role,
		AccountFingerprint: payload.AccountFingerprint,
		Nonce:              payload.Nonce,
		Sig:                payload.Sig,
	})
	if err != nil {
		return fmt.Errorf("enroll: tracker RPC: %w", err)
	}

	// All RPC work succeeded — only now do we touch disk.
	if opts.ReEnroll {
		if rmErr := os.Remove(keyPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("enroll: remove existing key for re-enroll: %w", rmErr)
		}
	}
	if err := identity.SaveKey(keyPath, signer); err != nil {
		return fmt.Errorf("enroll: save key: %w", err)
	}

	now := opts.Now().UTC()
	rec := &identity.Record{
		Version:            1,
		IdentityID:         resp.IdentityID,
		AccountFingerprint: fingerprint,
		Role:               opts.Role,
		EnrolledAt:         now,
	}
	if err := identity.SaveRecord(filepath.Join(opts.ConfigDir, "identity.json"), rec); err != nil {
		return fmt.Errorf("enroll: save identity record: %w", err)
	}

	em := &enrollmentMeta{
		Version:             enrollmentSchemaVersion,
		TrackerID:           opts.TrackerHash,
		Region:              opts.Region,
		StarterGrantCredits: resp.StarterGrantCredits,
		EnrolledAt:          now,
	}
	if err := saveEnrollmentMeta(filepath.Join(opts.ConfigDir, enrollmentFilename), em); err != nil {
		return fmt.Errorf("enroll: save enrollment metadata: %w", err)
	}

	if err := appendStarterGrant(opts.AuditLogPath, rec.IdentityID, resp.StarterGrantCredits, now); err != nil {
		return fmt.Errorf("enroll: record starter grant: %w", err)
	}

	if opts.Stderr != nil {
		fmt.Fprintf(opts.Stderr,
			"enroll: bound identity %x to tracker %x (region %q); starter grant %d credits\n",
			rec.IdentityID[:8], opts.TrackerHash[:8], opts.Region, resp.StarterGrantCredits)
	}
	return nil
}

func validateEnrollOptions(o enrollOptions) error {
	switch {
	case o.ConfigDir == "":
		return errors.New("enroll: ConfigDir empty")
	case o.AuditLogPath == "":
		return errors.New("enroll: AuditLogPath empty")
	case o.AuthProber == nil:
		return errors.New("enroll: AuthProber nil")
	case o.NewClient == nil:
		return errors.New("enroll: NewClient nil")
	case o.Now == nil:
		return errors.New("enroll: Now nil")
	}
	return nil
}

// preflightKeyPath enforces "fresh enroll requires no existing key" and
// "re-enroll permits but does not require an existing key."
func preflightKeyPath(keyPath string, reEnroll bool) error {
	_, err := os.Stat(keyPath)
	switch {
	case err == nil:
		if !reEnroll {
			return fmt.Errorf("enroll: identity already exists at %s; pass --re-enroll to rotate (does not preserve credits)", keyPath)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("enroll: stat %s: %w", keyPath, err)
	}
}

// appendStarterGrant records the cold-start grant per architecture spec
// §5.4. The grant is encoded as a ConsumerRecord with negative CostCredits
// (a credit gained, not spent) and ServedLocally=true (no seeder
// involved). Plugin spec §8 does not yet define a dedicated kind; until
// it does, this synthetic-consumer-record approach keeps the audit log
// inspectable via /token-bay logs without expanding the auditlog public
// surface.
func appendStarterGrant(path string, id [32]byte, credits uint64, ts time.Time) error {
	al, err := auditlog.Open(path)
	if err != nil {
		return err
	}
	defer al.Close()

	if credits > 1<<62 {
		return fmt.Errorf("starter grant credits %d overflow audit log int64", credits)
	}

	rec := auditlog.ConsumerRecord{
		RequestID:     "starter-grant:" + hex.EncodeToString(id[:]),
		ServedLocally: true,
		CostCredits:   -int64(credits),
		Timestamp:     ts,
	}
	return al.LogConsumer(rec)
}

// enrollmentMeta is the cmd-layer-owned sidecar at <config-dir>/enrollment.json.
// identity.Open does not consume it today — it is a separate JSON file that
// records which tracker issued the binding, the region, and the starter
// grant amount, all of which are outside identity.Record's v1 schema.
type enrollmentMeta struct {
	Version             int
	TrackerID           [32]byte
	Region              string
	StarterGrantCredits uint64
	EnrolledAt          time.Time
}

type enrollmentMetaWire struct {
	Version             int    `json:"version"`
	TrackerID           string `json:"tracker_id"`
	Region              string `json:"region"`
	StarterGrantCredits uint64 `json:"starter_grant_credits"`
	EnrolledAt          string `json:"enrolled_at"`
}

func saveEnrollmentMeta(path string, em *enrollmentMeta) error {
	w := enrollmentMetaWire{
		Version:             em.Version,
		TrackerID:           hex.EncodeToString(em.TrackerID[:]),
		Region:              em.Region,
		StarterGrantCredits: em.StarterGrantCredits,
		EnrolledAt:          em.EnrolledAt.UTC().Format(time.RFC3339Nano),
	}
	blob, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tmp := path + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadEnrollmentMeta(path string) (*enrollmentMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w enrollmentMetaWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("enrollment.json: %w", err)
	}
	if w.Version != enrollmentSchemaVersion {
		return nil, fmt.Errorf("enrollment.json: version %d, want %d", w.Version, enrollmentSchemaVersion)
	}
	rawID, err := hex.DecodeString(w.TrackerID)
	if err != nil {
		return nil, fmt.Errorf("enrollment.json tracker_id: %w", err)
	}
	if len(rawID) != 32 {
		return nil, fmt.Errorf("enrollment.json tracker_id: length %d, want 32", len(rawID))
	}
	var tid [32]byte
	copy(tid[:], rawID)
	enrolledAt, err := time.Parse(time.RFC3339Nano, w.EnrolledAt)
	if err != nil {
		return nil, fmt.Errorf("enrollment.json enrolled_at: %w", err)
	}
	return &enrollmentMeta{
		Version:             w.Version,
		TrackerID:           tid,
		Region:              w.Region,
		StarterGrantCredits: w.StarterGrantCredits,
		EnrolledAt:          enrolledAt.UTC(),
	}, nil
}

// constClientFactory wraps a pre-built EnrollClient as a NewClient factory.
// Tests use it; production constructs a fresh trackerclient.Client per call
// because the signer is generated inside runEnroll.
func constClientFactory(c EnrollClient) func(context.Context, *identity.Signer) (EnrollClient, func(), error) {
	return func(_ context.Context, _ *identity.Signer) (EnrollClient, func(), error) {
		return c, func() {}, nil
	}
}

func newEnrollCmd() *cobra.Command {
	var configPath string
	var roleFlag string
	var reEnroll bool
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "First-run identity enrollment with the configured tracker",
		Long: `Generates an Ed25519 keypair under <config-dir>/identity.key, captures the
account fingerprint via 'claude auth status --json', signs the canonical
'token-bay-enroll:v1' preimage, and exchanges it with the configured tracker
for a starter grant (plugin spec §4).

Refused on Bedrock, Vertex, or Foundry — orgId is not exposed under non-
firstParty providers and the /usage probe required for consumer fallback
needs claude.ai OAuth.

The --re-enroll flag rotates the keypair. Token-Bay v1 does NOT preserve
existing credits across rotations; the new identity starts with whatever
starter grant the tracker issues.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			endpoints, err := resolveTrackerEndpoints(cfg.Tracker, filepath.Dir(configPath), time.Now())
			if err != nil {
				return err
			}
			role, err := parseRole(roleFlag)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			logger := zerolog.New(cmd.ErrOrStderr()).With().Timestamp().Logger()
			factory := func(fctx context.Context, s *identity.Signer) (EnrollClient, func(), error) {
				tc, err := trackerclient.New(trackerclient.Config{
					Endpoints: endpoints,
					Identity:  s,
					Logger:    logger,
				})
				if err != nil {
					return nil, nil, fmt.Errorf("trackerclient: %w", err)
				}
				if err := tc.Start(fctx); err != nil {
					_ = tc.Close()
					return nil, nil, fmt.Errorf("trackerclient start: %w", err)
				}
				waitCtx, cancel := context.WithTimeout(fctx, connectTimeout)
				defer cancel()
				if err := tc.WaitConnected(waitCtx); err != nil {
					_ = tc.Close()
					return nil, nil, fmt.Errorf("connect tracker %s: %w", cfg.Tracker, err)
				}
				return tc, func() { _ = tc.Close() }, nil
			}

			return runEnroll(ctx, enrollOptions{
				ConfigDir:    filepath.Dir(configPath),
				AuditLogPath: cfg.AuditLogPath,
				Tracker:      cfg.Tracker,
				TrackerHash:  endpoints[0].IdentityHash,
				Region:       endpoints[0].Region,
				Role:         role,
				ReEnroll:     reEnroll,
				AuthProber:   ccproxy.NewClaudeAuthProber(),
				NewClient:    factory,
				Now:          time.Now,
				Stderr:       cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	cmd.Flags().StringVar(&roleFlag, "role", "consumer", "Role bitmask: consumer | seeder | both")
	cmd.Flags().BoolVar(&reEnroll, "re-enroll", false, "Rotate the existing keypair (does not preserve credits in v1)")
	return cmd
}

func parseRole(s string) (uint32, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "consumer":
		return identity.RoleConsumer, nil
	case "seeder":
		return identity.RoleSeeder, nil
	case "both":
		return identity.RoleConsumer | identity.RoleSeeder, nil
	default:
		return 0, fmt.Errorf("--role: unknown role %q (want consumer | seeder | both)", s)
	}
}
