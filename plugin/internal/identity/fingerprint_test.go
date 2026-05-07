package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const specExampleOrgID = "cd2c6c26-fdea-44af-b14f-11e283737e33"

func validSnapshot() AuthSnapshot {
	return AuthSnapshot{
		LoggedIn:    true,
		AuthMethod:  "claude.ai",
		APIProvider: "firstParty",
		OrgID:       specExampleOrgID,
	}
}

func TestAccountFingerprint_KnownVector(t *testing.T) {
	fp, err := AccountFingerprint(validSnapshot())
	require.NoError(t, err)

	expected := sha256.Sum256([]byte(specExampleOrgID))
	assert.Equal(t, expected, fp)

	// Lock as hex so we catch any future drift.
	assert.Equal(t, hex.EncodeToString(expected[:]), hex.EncodeToString(fp[:]))
}

func TestAccountFingerprint_LoggedInFalseRefused(t *testing.T) {
	s := validSnapshot()
	s.LoggedIn = false
	_, err := AccountFingerprint(s)
	require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_BedrockProviderRefused(t *testing.T) {
	s := validSnapshot()
	s.APIProvider = "bedrock"
	_, err := AccountFingerprint(s)
	require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_APIKeyMethodRefused(t *testing.T) {
	s := validSnapshot()
	s.AuthMethod = "api_key"
	_, err := AccountFingerprint(s)
	require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}

func TestAccountFingerprint_EmptyOrgIDRefused(t *testing.T) {
	s := validSnapshot()
	s.OrgID = ""
	_, err := AccountFingerprint(s)
	require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}
