// Command tokenbay-e2e-actor is a controllable consumer/seeder actor used by
// the tracker end-to-end test harness. It drives the tracker's wire protocol
// directly — identity, enroll, and (in later tasks) broker/offer/tunnel/
// settlement flows — WITHOUT a `claude` binary or any Anthropic key.
//
// This file is the flag/lifecycle entrypoint; the shared lifecycle lives in
// actor.go and the HTTP control surface in control.go.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/identity"
)

func main() {
	if err := realMain(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tokenbay-e2e-actor:", err)
		os.Exit(1)
	}
}

func realMain(args []string, stderr *os.File) error {
	fs := flag.NewFlagSet("tokenbay-e2e-actor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		roleFlag        = fs.String("role", "consumer", "actor role: consumer | seeder | both")
		trackerAddr     = fs.String("tracker-addr", "", "tracker A address host:port (UDP/QUIC)")
		trackerHashFile = fs.String("tracker-hash-file", "", "path to hex SPKI-hash file for tracker A (from e2egen)")
		trackerHash     = fs.String("tracker-hash", "", "hex SPKI-hash for tracker A (alternative to --tracker-hash-file)")
		trackerBAddr    = fs.String("tracker-b-addr", "", "tracker B address (consumer transfer target; used in a later task)")
		trackerBHashF   = fs.String("tracker-b-hash-file", "", "path to hex SPKI-hash file for tracker B (used in a later task)")
		trackerBHash    = fs.String("tracker-b-hash", "", "hex SPKI-hash for tracker B (alternative to --tracker-b-hash-file)")
		region          = fs.String("region", "A", "region hint for the tracker A endpoint")
		dataDir         = fs.String("data-dir", "", "directory for the actor's persistent identity key")
		ctrlAddr        = fs.String("ctrl-addr", "127.0.0.1:0", "HTTP control-API listen address")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	role, err := parseRole(*roleFlag)
	if err != nil {
		return err
	}
	if *trackerAddr == "" {
		return errors.New("--tracker-addr is required")
	}
	if *dataDir == "" {
		return errors.New("--data-dir is required")
	}

	hashA, err := resolveHash(*trackerHash, *trackerHashFile)
	if err != nil {
		return fmt.Errorf("tracker A hash: %w", err)
	}

	// Tracker B is the consumer's cross-region transfer target, consumed by
	// a later task. Resolve it opportunistically when provided so bad input
	// fails fast, but do not require it.
	var hashB [32]byte
	if *trackerBHash != "" || *trackerBHashF != "" {
		hashB, err = resolveHash(*trackerBHash, *trackerBHashF)
		if err != nil {
			return fmt.Errorf("tracker B hash: %w", err)
		}
	}

	logger := zerolog.New(stderr).With().Timestamp().Str("component", "e2e-actor").Logger()

	actor, err := newActor(options{
		Role:         role,
		RoleName:     strings.ToLower(strings.TrimSpace(*roleFlag)),
		TrackerAddr:  *trackerAddr,
		TrackerHash:  hashA,
		Region:       *region,
		TrackerBAddr: *trackerBAddr,
		TrackerBHash: hashB,
		DataDir:      *dataDir,
		CtrlAddr:     *ctrlAddr,
		Logger:       logger,
	})
	if err != nil {
		return err
	}

	logger.Info().Str("ctrl_addr", actor.CtrlAddr()).Str("role", *roleFlag).Msg("actor control API listening")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return actor.run(ctx)
}

// parseRole maps the --role string to the identity role bitmask.
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

// resolveHash returns the 32-byte tracker SPKI hash from either an inline hex
// value (--tracker-hash) or a hex file emitted by e2egen (--tracker-hash-file).
// The inline value wins when both are set.
func resolveHash(inlineHex, hexFile string) ([32]byte, error) {
	var out [32]byte
	raw := strings.TrimSpace(inlineHex)
	if raw == "" {
		if hexFile == "" {
			return out, errors.New("one of --tracker-hash / --tracker-hash-file is required")
		}
		b, err := os.ReadFile(hexFile)
		if err != nil {
			return out, fmt.Errorf("read hash file %q: %w", hexFile, err)
		}
		raw = strings.TrimSpace(string(b))
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return out, fmt.Errorf("decode hex: %w", err)
	}
	if len(decoded) != len(out) {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(decoded))
	}
	copy(out[:], decoded)
	return out, nil
}
