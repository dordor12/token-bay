package identity

import (
	"crypto/sha256"
	"fmt"
)

// Role bitmask values match tbproto.EnrollRequest.role.
const (
	RoleConsumer uint32 = 1 << 0
	RoleSeeder   uint32 = 1 << 1
)

// AuthSnapshot is the identity-relevant subset of `claude auth status --json`.
// Production callers map *ccproxy.AuthState onto this struct at the call
// site; tests construct it directly. Keeping the dependency one-way
// prevents an identity↔ccproxy import cycle.
type AuthSnapshot struct {
	LoggedIn    bool
	AuthMethod  string // "claude.ai" required
	APIProvider string // "firstParty" required
	OrgID       string // non-empty UUID required
}

// AccountFingerprint computes SHA-256(orgId) per plugin spec §4.2 step 4.
// Returns ErrIneligibleAuth when the snapshot fails the eligibility check.
func AccountFingerprint(s AuthSnapshot) ([32]byte, error) {
	if !s.LoggedIn {
		return [32]byte{}, fmt.Errorf("%w: not logged in", ErrIneligibleAuth)
	}
	if s.AuthMethod != "claude.ai" {
		return [32]byte{}, fmt.Errorf("%w: auth method %q (want claude.ai)", ErrIneligibleAuth, s.AuthMethod)
	}
	if s.APIProvider != "firstParty" {
		return [32]byte{}, fmt.Errorf("%w: api provider %q (want firstParty)", ErrIneligibleAuth, s.APIProvider)
	}
	if s.OrgID == "" {
		return [32]byte{}, fmt.Errorf("%w: missing orgId", ErrIneligibleAuth)
	}
	return sha256.Sum256([]byte(s.OrgID)), nil
}
