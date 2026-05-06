package admission

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestSnapshot_WriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.42")

	consumers := map[ids.IdentityID]*ConsumerCreditState{
		makeID(0xC1): {
			FirstSeenAt:     time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
			LastBalanceSeen: 1000,
		},
	}
	consumers[makeID(0xC1)].SettlementBuckets[0] = DayBucket{Total: 5, A: 5, DayStamp: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)}

	seeders := map[ids.IdentityID]*SeederHeartbeatState{
		makeID(0x5E): {
			LastHeadroomEstimate: 7000,
			LastHeadroomSource:   2,
			LastHeadroomTs:       time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
			CanProbeUsage:        true,
		},
	}

	require.NoError(t, writeSnapshot(path, 42, time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), consumers, seeders))

	snap, err := readSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), snap.Seq)
	require.Len(t, snap.Consumers, 1)
	require.Len(t, snap.Seeders, 1)

	cs := snap.Consumers[makeID(0xC1)]
	require.NotNil(t, cs)
	assert.Equal(t, int64(1000), cs.LastBalanceSeen)
	assert.Equal(t, uint32(5), cs.SettlementBuckets[0].Total)

	ss := snap.Seeders[makeID(0x5E)]
	require.NotNil(t, ss)
	assert.Equal(t, uint32(7000), ss.LastHeadroomEstimate)
	assert.True(t, ss.CanProbeUsage)
}

func TestSnapshot_AtomicWrite_NoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, writeSnapshot(path, 1, time.Now(), nil, nil))
	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "tmp file should not survive successful rename")
}

func TestSnapshot_BadMagic_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, os.WriteFile(path, []byte{
		0xff, 0xff, 0xff, 0xff, 1, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}, 0o644))

	_, err := readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "magic")
}

func TestSnapshot_BadFormatVersion_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	buf := make([]byte, 28)
	binary.LittleEndian.PutUint32(buf[0:4], snapshotMagic)
	binary.LittleEndian.PutUint32(buf[4:8], 99)
	require.NoError(t, os.WriteFile(path, buf, 0o644))

	_, err := readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "format_version")
}

func TestSnapshot_TamperedTrailer_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, writeSnapshot(path, 1, time.Now(), nil, nil))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o644))

	_, err = readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailer")
}

func TestEnumerateSnapshots_OrdersBySeq(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "admission.snapshot")
	for _, seq := range []uint64{50, 100, 75, 200} {
		require.NoError(t, writeSnapshot(fmt.Sprintf("%s.%d", prefix, seq), seq, time.Now(), nil, nil))
	}
	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	require.Len(t, files, 4)
	assert.Equal(t, uint64(50), files[0].seq)
	assert.Equal(t, uint64(75), files[1].seq)
	assert.Equal(t, uint64(100), files[2].seq)
	assert.Equal(t, uint64(200), files[3].seq)
}
