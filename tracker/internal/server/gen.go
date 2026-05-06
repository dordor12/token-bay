//go:build ignore

// gen.go regenerates the Ed25519 keypairs in testdata/. Run with:
//
//	go run ./internal/server/gen.go -out tracker/internal/server/testdata
//
// from the repo root. Committed keys are stable test fixtures —
// regeneration changes every test that references the SPKI hash.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "testdata", "output directory")
	flag.Parse()
	for _, name := range []string{"server", "client"} {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		path := filepath.Join(*out, name+"_ed25519.key")
		if err := os.WriteFile(path, priv, 0o600); err != nil {
			panic(err)
		}
	}
}
