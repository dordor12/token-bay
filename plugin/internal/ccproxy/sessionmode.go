package ccproxy

import (
	"crypto/ed25519"
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// SessionMode classifies the routing path for a given session.
type SessionMode int

const (
	ModePassThrough SessionMode = iota
	ModeNetwork
)

// EntryMetadata is the per-session state carried for ModeNetwork sessions.
type EntryMetadata struct {
	EnteredAt          time.Time
	ExpiresAt          time.Time
	StopFailurePayload *ratelimit.StopFailurePayload
	UsageProbeBytes    []byte
	UsageVerdict       ratelimit.UsageVerdict

	// Seeder routing — populated by the StopFailure-hook orchestrator
	// (separate plan) when activating ModeNetwork. NetworkRouter reads
	// these via PeerDialer to dial the seeder.
	SeederAddr    netip.AddrPort
	SeederPubkey  ed25519.PublicKey
	EphemeralPriv ed25519.PrivateKey
}

// SessionModeStore is a thread-safe in-memory map of session ID → entry.
type SessionModeStore struct {
	mu      sync.RWMutex
	entries map[string]*EntryMetadata
}

// NewSessionModeStore returns an empty store.
func NewSessionModeStore() *SessionModeStore {
	return &SessionModeStore{entries: make(map[string]*EntryMetadata)}
}

// EnterNetworkMode records meta for sessionID. Replaces any prior entry.
func (s *SessionModeStore) EnterNetworkMode(sessionID string, meta EntryMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := meta
	s.entries[sessionID] = &entry
}

// ExitNetworkMode removes the entry for sessionID. Returns true if an
// entry was present.
func (s *SessionModeStore) ExitNetworkMode(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[sessionID]
	if ok {
		delete(s.entries, sessionID)
	}
	return ok
}

// GetMode returns the current routing mode for sessionID. An expired
// entry (ExpiresAt < now) is reported as PassThrough; the stale entry
// remains in the map until a background reaper removes it (not in v1;
// sidecar orchestrator calls ExitNetworkMode on timer).
func (s *SessionModeStore) GetMode(sessionID string) (SessionMode, *EntryMetadata) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[sessionID]
	if !ok {
		return ModePassThrough, nil
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		return ModePassThrough, nil
	}
	// Return a copy so callers can't mutate internal state.
	out := *entry
	return ModeNetwork, &out
}
