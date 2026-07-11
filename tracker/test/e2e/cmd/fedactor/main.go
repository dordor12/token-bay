package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	privKeyPath := flag.String("priv-key-path", "", "path to a raw 64-byte ed25519 private key file (e.g. e2egen identity-fed.key)")
	ctrlAddr := flag.String("ctrl-addr", "0.0.0.0:8080", "listen address for the HTTP control API")
	flag.Parse()

	if *privKeyPath == "" {
		log.Fatal("fedactor: --priv-key-path is required")
	}
	priv, err := loadPrivKey(*privKeyPath)
	if err != nil {
		log.Fatalf("fedactor: load key: %v", err)
	}
	actor, err := NewActor(priv)
	if err != nil {
		log.Fatalf("fedactor: %v", err)
	}
	fedID := actor.FedID()
	log.Printf("fedactor: identity loaded, tracker_id=%s ctrl=%s", hex.EncodeToString(fedID[:]), *ctrlAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := newControlServer(actor).listenAndServe(ctx, *ctrlAddr); err != nil {
		log.Fatalf("fedactor: control server: %v", err)
	}
}

// loadPrivKey reads a raw 64-byte ed25519 private key from disk. This matches
// the format e2egen writes for identity-fed.key.
func loadPrivKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, os.ErrInvalid
	}
	return ed25519.PrivateKey(raw), nil
}
