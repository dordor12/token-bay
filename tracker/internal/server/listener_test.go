package server_test

import (
	"strings"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

func TestStartListener_BindsAndCloses(t *testing.T) {
	priv := loadKey(t, "server")
	cert, err := server.ServerCertFromIdentity(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.ServerConfig{
		ListenAddr:         "127.0.0.1:0",
		IdleTimeoutS:       60,
		MaxIncomingStreams: 1024,
		MaxFrameSize:       1 << 20,
	}
	l, err := server.StartListener(cfg, cert)
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr() == nil {
		t.Fatal("nil addr")
	}
	if !strings.Contains(l.Addr().String(), "127.0.0.1:") {
		t.Fatalf("listening at %v", l.Addr())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartListener_RejectsBadAddr(t *testing.T) {
	priv := loadKey(t, "server")
	cert, _ := server.ServerCertFromIdentity(priv)
	cfg := &config.ServerConfig{
		ListenAddr:         "not-a-host-port",
		IdleTimeoutS:       60,
		MaxIncomingStreams: 1024,
		MaxFrameSize:       1 << 20,
	}
	if _, err := server.StartListener(cfg, cert); err == nil {
		t.Fatal("want error on bad ListenAddr")
	}
}
