package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "e2egen: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("e2egen", flag.ContinueOnError)
	outDir := fs.String("out", "", "output directory for keys and configs (required)")
	seedAHex := fs.String("seed-a", "", "32-byte hex Ed25519 seed for tracker-a's identity (required)")
	seedBHex := fs.String("seed-b", "", "32-byte hex Ed25519 seed for tracker-b's identity (required)")
	seedFedHex := fs.String("seed-fed", "", "32-byte hex Ed25519 seed for the fedactor identity (required)")
	addrA := fs.String("addr-a", "tracker-a", "hostname tracker-b dials to reach tracker-a's federation listener")
	addrB := fs.String("addr-b", "tracker-b", "hostname tracker-a dials to reach tracker-b's federation listener")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *outDir == "" {
		return fmt.Errorf("--out is required")
	}

	seedA, err := parseSeed("seed-a", *seedAHex)
	if err != nil {
		return err
	}
	seedB, err := parseSeed("seed-b", *seedBHex)
	if err != nil {
		return err
	}
	seedFed, err := parseSeed("seed-fed", *seedFedHex)
	if err != nil {
		return err
	}

	return generate(genOpts{
		OutDir:  *outDir,
		SeedA:   seedA,
		SeedB:   seedB,
		SeedFed: seedFed,
		AddrA:   *addrA,
		AddrB:   *addrB,
	})
}

// parseSeed decodes a hex-encoded Ed25519 seed for flagName, rejecting
// anything that isn't exactly ed25519.SeedSize (32) bytes.
func parseSeed(flagName, hexSeed string) ([]byte, error) {
	if hexSeed == "" {
		return nil, fmt.Errorf("--%s is required", flagName)
	}
	seed, err := hex.DecodeString(hexSeed)
	if err != nil {
		return nil, fmt.Errorf("--%s: invalid hex: %w", flagName, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("--%s: must be %d bytes, got %d", flagName, ed25519.SeedSize, len(seed))
	}
	return seed, nil
}
