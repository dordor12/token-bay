package ccproxy

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionModeStore_EmptyGet_ReturnsPassThrough(t *testing.T) {
	s := NewSessionModeStore()
	mode, meta := s.GetMode("session-x")
	assert.Equal(t, ModePassThrough, mode)
	assert.Nil(t, meta)
}

func TestSessionModeStore_EnterThenGet_ReturnsNetwork(t *testing.T) {
	s := NewSessionModeStore()
	meta := EntryMetadata{
		EnteredAt: time.Now(),
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	s.EnterNetworkMode("session-x", meta)

	mode, got := s.GetMode("session-x")
	require.Equal(t, ModeNetwork, mode)
	require.NotNil(t, got)
	assert.Equal(t, meta.ExpiresAt, got.ExpiresAt)
}

func TestSessionModeStore_Expired_ReturnsPassThrough(t *testing.T) {
	s := NewSessionModeStore()
	s.EnterNetworkMode("session-x", EntryMetadata{
		EnteredAt: time.Now().Add(-20 * time.Minute),
		ExpiresAt: time.Now().Add(-5 * time.Minute),
	})
	mode, _ := s.GetMode("session-x")
	assert.Equal(t, ModePassThrough, mode)
}

func TestSessionModeStore_Exit_Existing_ReturnsTrue(t *testing.T) {
	s := NewSessionModeStore()
	s.EnterNetworkMode("s", EntryMetadata{ExpiresAt: time.Now().Add(time.Minute)})
	assert.True(t, s.ExitNetworkMode("s"))
	assert.False(t, s.ExitNetworkMode("s")) // idempotent
}

func TestSessionModeStore_Exit_Missing_ReturnsFalse(t *testing.T) {
	s := NewSessionModeStore()
	assert.False(t, s.ExitNetworkMode("never-entered"))
}

func TestSessionModeStore_Concurrent_Safe(t *testing.T) {
	s := NewSessionModeStore()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := "s" + string(rune('a'+i%26))
			s.EnterNetworkMode(sid, EntryMetadata{ExpiresAt: time.Now().Add(time.Minute)})
			_, _ = s.GetMode(sid)
			s.ExitNetworkMode(sid)
		}(i)
	}
	wg.Wait()
	// No panic / data race — test passes under -race if we got here.
}
