package main

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/reputation"
)

// openE2EReputation opens a real SQLite-backed reputation.Subsystem in a
// tempdir — the same wiring run_cmd.go performs at startup — so adapter
// tests exercise the actual Freeze/Unfreeze state machine rather than a
// fake.
func openE2EReputation(t *testing.T) *reputation.Subsystem {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Reputation.StoragePath = filepath.Join(t.TempDir(), "reputation.db")
	rep, err := reputation.Open(context.Background(), cfg.Reputation)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rep.Close() })
	return rep
}

func mkAdapterID(b byte) ids.IdentityID {
	var id ids.IdentityID
	for i := range id {
		id[i] = b
	}
	return id
}

func TestReputationAdminActions_Freeze_Success(t *testing.T) {
	rep := openE2EReputation(t)
	a := reputationAdminActions{rep: rep}

	id := mkAdapterID(0x11)
	idHex := hex.EncodeToString(id[:])

	require.NoError(t, a.Freeze(idHex, "admin"))
	assert.True(t, rep.IsFrozen(id))
}

func TestReputationAdminActions_Unfreeze_Success(t *testing.T) {
	rep := openE2EReputation(t)
	a := reputationAdminActions{rep: rep}

	id := mkAdapterID(0x22)
	idHex := hex.EncodeToString(id[:])

	require.NoError(t, a.Freeze(idHex, "admin"))
	require.True(t, rep.IsFrozen(id))

	require.NoError(t, a.Unfreeze(idHex, "admin"))
	assert.False(t, rep.IsFrozen(id))
}

func TestReputationAdminActions_Freeze_BadHex(t *testing.T) {
	rep := openE2EReputation(t)
	a := reputationAdminActions{rep: rep}

	err := a.Freeze("not-hex-at-all", "admin")
	assert.Error(t, err)
}

func TestReputationAdminActions_Freeze_WrongLength(t *testing.T) {
	rep := openE2EReputation(t)
	a := reputationAdminActions{rep: rep}

	// Valid hex, but short of the required 32 bytes.
	err := a.Freeze("aabbcc", "admin")
	assert.Error(t, err)
}

func TestReputationAdminActions_Unfreeze_BadHex(t *testing.T) {
	rep := openE2EReputation(t)
	a := reputationAdminActions{rep: rep}

	err := a.Unfreeze("zz", "admin")
	assert.Error(t, err)
}
