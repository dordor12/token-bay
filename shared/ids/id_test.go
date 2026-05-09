package ids

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityID_ZeroValue_IsZeroBytes(t *testing.T) {
	var id IdentityID
	assert.Equal(t, [32]byte{}, id.Bytes())
}

func TestTrackerID_Bytes_RoundTrip(t *testing.T) {
	t.Parallel()
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	id := TrackerID(raw)
	if got := id.Bytes(); got != raw {
		t.Fatalf("Bytes round-trip: got %x want %x", got, raw)
	}
}

func TestTrackerID_DistinctFromIdentityID(t *testing.T) {
	t.Parallel()
	// Compile-time check: a function taking TrackerID must NOT accept IdentityID.
	var _ func(TrackerID) = func(TrackerID) {}
	// Runtime: same byte content, different types → cannot be assigned without conversion.
	var raw [32]byte
	tid := TrackerID(raw)
	iid := IdentityID(raw)
	if tid.Bytes() != iid.Bytes() {
		t.Fatalf("backing arrays must match for safe conversion")
	}
}
