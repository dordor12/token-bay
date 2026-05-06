package admission

import (
	"context"
	"time"
)

type ctxKey int

const operatorIDKey ctxKey = 1

// WithOperatorContext stamps the request's operator identity onto ctx.
// Callers (admin handlers + tests) chain this when constructing a
// request context that should carry an audit trail.
func WithOperatorContext(ctx context.Context, operatorID string) context.Context {
	return context.WithValue(ctx, operatorIDKey, operatorID)
}

// operatorIDFromContext returns the bound operator id or "anonymous"
// when the key is missing.
func operatorIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(operatorIDKey).(string); ok && v != "" {
		return v
	}
	return "anonymous"
}

// writeOperatorOverride appends an OPERATOR_OVERRIDE record. Returns nil
// (and is a no-op) when the tlog is disabled.
func (s *Subsystem) writeOperatorOverride(ctx context.Context, action string, paramsJSON []byte) error {
	if s.tlog == nil {
		return nil
	}
	p := OperatorOverridePayload{
		OperatorID: operatorIDFromContext(ctx),
		Action:     action,
		Params:     paramsJSON,
		Ts:         s.nowFn().Unix(),
	}
	body, err := p.MarshalBinary()
	if err != nil {
		return err
	}
	return s.tlog.Append(TLogRecord{
		Seq:     s.nextSeq.Add(1),
		Kind:    TLogKindOperatorOverride,
		Payload: body,
		Ts:      uint64(time.Unix(p.Ts, 0).UTC().Unix()), //nolint:gosec // G115 — post-1970
	})
}
