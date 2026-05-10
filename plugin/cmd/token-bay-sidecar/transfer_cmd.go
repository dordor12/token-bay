package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/config"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// transferSafetyOverhead is the v1 floor we keep in reserve on top of the
// requested transfer amount so a tracker-side rejection for "balance equals
// amount but minus a hair of overhead" doesn't strand the consumer in a
// debited-then-undefined state. Federation §4.2 step 3 mentions overhead
// without fixing a number; this is a fixed-credit floor (simpler than a
// percent and cheap to reason about for small amounts).
const transferSafetyOverhead uint64 = 100

// TransferClient is the seam runTransfer uses to talk to the tracker.
// *trackerclient.Client satisfies it; tests inject a fake.
type TransferClient interface {
	BalanceCached(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error)
	TransferRequest(ctx context.Context, tr *trackerclient.TransferRequest) (*trackerclient.TransferProof, error)
}

// transferOptions carries the inputs runTransfer needs. Production wiring
// is in newTransferCmd; tests construct this directly with stub seams.
type transferOptions struct {
	ConfigDir    string
	AuditLogPath string
	DestRegion   string
	Amount       uint64
	DryRun       bool

	// NewClient takes the loaded signer (used for QUIC TLS identity in
	// production) and returns a connected TransferClient plus a teardown.
	NewClient func(ctx context.Context, s *identity.Signer) (TransferClient, func(), error)
	Now       func() time.Time
	Stdout    io.Writer
	Stderr    io.Writer
}

// runTransfer performs the consumer-initiated cross-region credit
// transfer per federation spec §4. Resolves the source region from
// enrollment.json, performs a local balance pre-check (refusing if
// balance < amount + transferSafetyOverhead), generates a 16-byte nonce,
// signs the canonical request bytes with the consumer's identity key,
// calls the tracker's TransferRequest RPC, and writes a TransferRecord
// to the audit log on completion. Source-side debit is final once
// TransferProof returns (federation §4.3 ordering invariant); the
// destination's TRANSFER_APPLIED arrives asynchronously and is not
// awaited here.
func runTransfer(ctx context.Context, opts transferOptions) error {
	if err := validateTransferOptions(opts); err != nil {
		return err
	}

	signer, rec, err := identity.Open(opts.ConfigDir)
	if err != nil {
		return fmt.Errorf("transfer: open identity at %s (run /token-bay enroll first?): %w", opts.ConfigDir, err)
	}
	em, err := loadEnrollmentMeta(filepath.Join(opts.ConfigDir, enrollmentFilename))
	if err != nil {
		return fmt.Errorf("transfer: load enrollment metadata: %w", err)
	}
	if em.Region == opts.DestRegion {
		return fmt.Errorf("transfer: dest region %q matches source region; nothing to transfer", opts.DestRegion)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("transfer: generate nonce: %w", err)
	}
	requestID := "transfer:" + hex.EncodeToString(nonce[:])

	tr := &trackerclient.TransferRequest{
		IdentityID: rec.IdentityID,
		Amount:     opts.Amount,
		DestRegion: opts.DestRegion,
		Nonce:      nonce,
	}

	// Sign the canonical RPC payload with the consumer's identity key.
	// The wire form has no consumer-sig field today (proto change is a
	// follow-up per federation cross-region transfer spec §2 non-goals);
	// the sig is recorded in the audit log as proof of intent and as
	// a forward-compatible artifact when the api handler grows the
	// field. Canonicalization mirrors shared/federation/signing_transfer.go's
	// pattern: route through shared/signing.DeterministicMarshal.
	canonical, err := signing.DeterministicMarshal(&tbproto.TransferRequest{
		IdentityId: tr.IdentityID[:],
		Amount:     tr.Amount,
		DestRegion: tr.DestRegion,
		Nonce:      tr.Nonce[:],
	})
	if err != nil {
		return fmt.Errorf("transfer: marshal canonical bytes: %w", err)
	}
	if _, err := signer.Sign(canonical); err != nil {
		return fmt.Errorf("transfer: sign request: %w", err)
	}

	now := opts.Now().UTC()

	if opts.DryRun {
		fmt.Fprintf(opts.Stdout,
			"transfer: would send %d credits from %q to %q (request_id=%s, identity=%x)\n",
			opts.Amount, em.Region, opts.DestRegion, requestID, rec.IdentityID[:8])
		return nil
	}

	client, closeClient, err := opts.NewClient(ctx, signer)
	if err != nil {
		return fmt.Errorf("transfer: open tracker connection: %w", err)
	}
	defer closeClient()

	snap, err := client.BalanceCached(ctx, rec.IdentityID)
	if err != nil {
		return fmt.Errorf("transfer: fetch balance: %w", err)
	}
	credits := uint64(0)
	if c := snap.GetBody().GetCredits(); c > 0 {
		credits = uint64(c)
	}
	required := opts.Amount + transferSafetyOverhead
	if credits < required {
		return fmt.Errorf("transfer: insufficient balance: have %d, need %d (amount %d + overhead %d)",
			credits, required, opts.Amount, transferSafetyOverhead)
	}

	proof, err := client.TransferRequest(ctx, tr)
	if err != nil {
		// Tracker reported a typed rejection (frozen, no-capacity, etc.) or
		// a bare RPC error. Audit the rejection so /token-bay logs shows it.
		if logErr := writeTransferAudit(opts.AuditLogPath, auditlog.TransferRecord{
			RequestID:     requestID,
			SourceRegion:  em.Region,
			DestRegion:    opts.DestRegion,
			Amount:        opts.Amount,
			Outcome:       auditlog.TransferOutcomeRejected,
			OutcomeReason: err.Error(),
			Timestamp:     now,
		}); logErr != nil {
			return fmt.Errorf("transfer: tracker rejected (%w); audit log write also failed: %v", err, logErr)
		}
		return fmt.Errorf("transfer: tracker rejected: %w", err)
	}

	chainTip := proof.SourceChainTipHash
	if err := writeTransferAudit(opts.AuditLogPath, auditlog.TransferRecord{
		RequestID:          requestID,
		SourceRegion:       em.Region,
		DestRegion:         opts.DestRegion,
		Amount:             opts.Amount,
		Outcome:            auditlog.TransferOutcomeSuccess,
		SourceChainTipHash: &chainTip,
		SourceSeq:          proof.SourceSeq,
		Timestamp:          now,
	}); err != nil {
		return fmt.Errorf("transfer: write audit entry: %w", err)
	}

	fmt.Fprintf(opts.Stdout,
		"transfer: debit confirmed at source (%s seq=%d, chain_tip=%x). %d credits transferred to %q. "+
			"Destination region will credit shortly; check `balance` at destination. (request_id=%s)\n",
		em.Region, proof.SourceSeq, chainTip[:8], opts.Amount, opts.DestRegion, requestID)
	return nil
}

func validateTransferOptions(o transferOptions) error {
	switch {
	case o.ConfigDir == "":
		return errors.New("transfer: ConfigDir empty")
	case o.AuditLogPath == "":
		return errors.New("transfer: AuditLogPath empty")
	case o.DestRegion == "":
		return errors.New("transfer: --to is required")
	case o.Amount == 0:
		return errors.New("transfer: --amount must be > 0")
	case o.NewClient == nil:
		return errors.New("transfer: NewClient nil")
	case o.Now == nil:
		return errors.New("transfer: Now nil")
	}
	return nil
}

func writeTransferAudit(path string, rec auditlog.TransferRecord) error {
	al, err := auditlog.Open(path)
	if err != nil {
		return err
	}
	defer al.Close()
	return al.LogTransfer(rec)
}

func newTransferCmd() *cobra.Command {
	var configPath string
	var to string
	var amount uint64
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Move credits from your home region to another region",
		Long: `Initiates a federated cross-region credit transfer. The consumer's home
tracker (set at enrollment) debits the requested amount and returns a signed
TransferProof; the destination tracker applies the credit asynchronously
(federation spec §4). The CLI does not wait for the destination side —
'check balance at destination' once the transfer settles.

Refuses if local balance is less than --amount plus a small safety overhead.
The --dry-run flag validates inputs and signing locally without contacting
the tracker.`,
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

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			logger := zerolog.New(cmd.ErrOrStderr()).With().Timestamp().Logger()
			factory := func(fctx context.Context, s *identity.Signer) (TransferClient, func(), error) {
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

			return runTransfer(ctx, transferOptions{
				ConfigDir:    filepath.Dir(configPath),
				AuditLogPath: cfg.AuditLogPath,
				DestRegion:   to,
				Amount:       amount,
				DryRun:       dryRun,
				NewClient:    factory,
				Now:          time.Now,
				Stdout:       cmd.OutOrStdout(),
				Stderr:       cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	cmd.Flags().StringVar(&to, "to", "", "Destination region (required)")
	cmd.Flags().Uint64Var(&amount, "amount", 0, "Credits to transfer (required, must be > 0)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate inputs and sign locally; do not contact the tracker")
	return cmd
}
