package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/admin"
	"github.com/token-bay/token-bay/tracker/internal/reputation"
)

// reputationAdminActions adapts *reputation.Subsystem into the
// admin.ReputationActions interface (P7 freeze/unfreeze). It is the
// single place that crosses the admin↔reputation package boundary:
// admin imports neither reputation types nor errors, and reputation
// imports nothing from admin.
type reputationAdminActions struct {
	rep *reputation.Subsystem
}

// Freeze decodes idHex into an ids.IdentityID and delegates to
// reputation.Subsystem.Freeze. A malformed idHex never reaches the
// subsystem — it is rejected here with a client-facing error.
func (r reputationAdminActions) Freeze(idHex, operator string) error {
	id, err := hexToIdentityID(idHex)
	if err != nil {
		return fmt.Errorf("identity id: %w", err)
	}
	return r.rep.Freeze(context.Background(), id, operator)
}

// Unfreeze decodes idHex into an ids.IdentityID and delegates to
// reputation.Subsystem.Unfreeze.
func (r reputationAdminActions) Unfreeze(idHex, operator string) error {
	id, err := hexToIdentityID(idHex)
	if err != nil {
		return fmt.Errorf("identity id: %w", err)
	}
	return r.rep.Unfreeze(context.Background(), id, operator)
}

// hexToIdentityID decodes a hex string into an ids.IdentityID, requiring
// exactly 32 bytes (64 hex chars) — the same convention as
// hexToTrackerID in federation_adapters.go.
func hexToIdentityID(s string) (ids.IdentityID, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return ids.IdentityID{}, fmt.Errorf("identity id must be 32 hex bytes")
	}
	var out ids.IdentityID
	copy(out[:], b)
	return out, nil
}

var _ admin.ReputationActions = reputationAdminActions{}
