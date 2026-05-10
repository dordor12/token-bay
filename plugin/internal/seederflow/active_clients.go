package seederflow

import (
	"crypto/ed25519"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
)

// IsActive reports whether the seeder is currently serving a request
// for the consumer identified by pub. Implements
// ccbridge.ActiveClientChecker. Returns false for nil/wrong-length
// keys.
func (c *Coordinator) IsActive(pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return c.IsActiveByHash(ccbridge.ClientHash(pub))
}

// IsActiveByHash reports whether the seeder is currently serving a
// request for the client whose pubkey hashes to hash. Implements
// ccbridge.ActiveByHashChecker.
func (c *Coordinator) IsActiveByHash(hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeClients[hash] > 0
}

// RegisterActive marks pub as actively being served. Reference-counted,
// so concurrent serves to the same client are safe.
func (c *Coordinator) RegisterActive(pub ed25519.PublicKey) {
	if len(pub) != ed25519.PublicKeySize {
		return
	}
	hash := ccbridge.ClientHash(pub)
	c.mu.Lock()
	c.activeClients[hash]++
	c.mu.Unlock()
}

// UnregisterActive decrements the reference count for pub. When it
// reaches zero, the entry is removed.
func (c *Coordinator) UnregisterActive(pub ed25519.PublicKey) {
	if len(pub) != ed25519.PublicKeySize {
		return
	}
	hash := ccbridge.ClientHash(pub)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeClients[hash] <= 1 {
		delete(c.activeClients, hash)
		return
	}
	c.activeClients[hash]--
}

// Compile-time interface checks.
var (
	_ ccbridge.ActiveClientChecker = (*Coordinator)(nil)
	_ ccbridge.ActiveByHashChecker = (*Coordinator)(nil)
)
