package federation

import (
	"testing"
	"time"
)

func TestConfig_WithDefaults_TransferTimeout(t *testing.T) {
	t.Parallel()
	c := (Config{}).withDefaults()
	if c.TransferTimeout == 0 {
		t.Fatalf("TransferTimeout default = 0, want non-zero")
	}
	if c.TransferTimeout < time.Second || c.TransferTimeout > 600*time.Second {
		t.Fatalf("TransferTimeout = %v out of [1s, 600s]", c.TransferTimeout)
	}
}

func TestConfig_WithDefaults_IssuedProofCap(t *testing.T) {
	t.Parallel()
	c := (Config{}).withDefaults()
	if c.IssuedProofCap == 0 {
		t.Fatalf("IssuedProofCap default = 0, want non-zero")
	}
	if c.IssuedProofCap < 128 {
		t.Fatalf("IssuedProofCap = %d, want >= 128", c.IssuedProofCap)
	}
}

func TestConfig_WithDefaults_PreservesUserOverrides(t *testing.T) {
	t.Parallel()
	c := (Config{
		TransferTimeout: 5 * time.Second,
		IssuedProofCap:  256,
	}).withDefaults()
	if c.TransferTimeout != 5*time.Second {
		t.Fatalf("TransferTimeout=%v, want 5s", c.TransferTimeout)
	}
	if c.IssuedProofCap != 256 {
		t.Fatalf("IssuedProofCap=%d, want 256", c.IssuedProofCap)
	}
}
