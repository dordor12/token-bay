package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTip_EmptyChain(t *testing.T) {
	s := openTempStore(t)
	seq, hash, ok, err := s.Tip(context.Background())
	require.NoError(t, err)
	assert.False(t, ok, "empty chain has no tip")
	assert.Zero(t, seq)
	assert.Nil(t, hash)
}

// Tip-after-AppendEntry behavior is exercised end-to-end in append_test.go,
// once AppendEntry exists. Here we lock only the empty-chain semantics so
// callers know not to special-case (err == nil, ok == false).
