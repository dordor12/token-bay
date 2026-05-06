package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperatorOverride_AppendsTLogRecord(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	ctx := WithOperatorContext(context.Background(), "operator-7")
	require.NoError(t, s.writeOperatorOverride(ctx, "queue/drain", []byte(`{"n":2}`)))
	require.NoError(t, s.tlog.Close())

	recs, _, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindOperatorOverride, recs[0].Kind)

	var p OperatorOverridePayload
	require.NoError(t, p.UnmarshalBinary(recs[0].Payload))
	assert.Equal(t, "operator-7", p.OperatorID)
	assert.Equal(t, "queue/drain", p.Action)
	assert.JSONEq(t, `{"n":2}`, string(p.Params))
}

func TestOperatorOverride_NoOpWhenTLogDisabled(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	ctx := WithOperatorContext(context.Background(), "operator-7")
	assert.NoError(t, s.writeOperatorOverride(ctx, "queue/drain", []byte(`{}`)))
}

func TestOperatorOverride_AnonymousWhenContextMissing(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	require.NoError(t, s.writeOperatorOverride(context.Background(), "snapshot", []byte("{}")))
	require.NoError(t, s.tlog.Close())

	recs, _, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	var p OperatorOverridePayload
	require.NoError(t, p.UnmarshalBinary(recs[0].Payload))
	assert.Equal(t, "anonymous", p.OperatorID)
}
