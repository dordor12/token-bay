package auditlog

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Record kinds. The discriminator field on every line.
const (
	KindConsumer = "consumer"
	KindSeeder   = "seeder"
	KindTransfer = "transfer"
)

// Transfer outcome values. Carried as the Outcome field on TransferRecord.
const (
	TransferOutcomeSuccess  = "success"
	TransferOutcomeRejected = "rejected"
)

// Record is the sealed sum type for audit-log entries. Implementations:
// ConsumerRecord (consumer-side per-request entry, plugin spec §8),
// SeederRecord (seeder-side per-request entry, plugin spec §8), and
// UnknownRecord (forward-compat envelope yielded by Read when a line's
// kind is not recognized).
type Record interface{ isRecord() }

// ConsumerRecord is appended on the consumer side after each fallback
// turn — whether served locally (no network) or via a seeder.
type ConsumerRecord struct {
	RequestID     string
	ServedLocally bool
	SeederID      string // empty when ServedLocally is true
	CostCredits   int64
	Timestamp     time.Time
}

// SeederRecord is appended on the seeder side after each forwarded
// request the local Claude Code bridge served. No prompt or response
// content is ever recorded — only metering metadata.
type SeederRecord struct {
	RequestID        string
	Model            string
	InputTokens      int
	OutputTokens     int
	ConsumerIDHash   [32]byte
	StartedAt        time.Time
	CompletedAt      time.Time
	TrackerEntryHash *[32]byte
}

// TransferRecord is appended on the consumer side after a cross-region
// credit transfer attempt. Plugin spec §8 does not yet define a dedicated
// kind for transfers; this record captures the federation-flow fields the
// /token-bay logs viewer needs (federation spec §4.2 + §4.3) without
// expanding ConsumerRecord. SourceChainTipHash and SourceSeq are unset on
// rejected attempts (no proof was minted at the source).
type TransferRecord struct {
	RequestID          string
	SourceRegion       string
	DestRegion         string
	Amount             uint64
	Outcome            string // TransferOutcomeSuccess | TransferOutcomeRejected
	OutcomeReason      string // empty on success
	SourceChainTipHash *[32]byte
	SourceSeq          uint64
	Timestamp          time.Time
}

// UnknownRecord wraps a JSON line whose kind discriminator was not one
// of the values this binary understands. Read yields one rather than
// erroring so a newer plugin's audit log stays readable by older tooling.
type UnknownRecord struct {
	Kind string
	Raw  json.RawMessage
}

func (ConsumerRecord) isRecord() {}
func (SeederRecord) isRecord()   {}
func (TransferRecord) isRecord() {}
func (UnknownRecord) isRecord()  {}

// marshalRecord renders rec as a single-line JSON object — no trailing newline.
func marshalRecord(rec Record) ([]byte, error) {
	switch r := rec.(type) {
	case ConsumerRecord:
		return json.Marshal(consumerWire{
			Kind:          KindConsumer,
			RequestID:     r.RequestID,
			ServedLocally: r.ServedLocally,
			SeederID:      r.SeederID,
			CostCredits:   r.CostCredits,
			Timestamp:     timeStr(r.Timestamp),
		})
	case SeederRecord:
		var trackerHash *string
		if r.TrackerEntryHash != nil {
			s := hex.EncodeToString(r.TrackerEntryHash[:])
			trackerHash = &s
		}
		return json.Marshal(seederWire{
			Kind:             KindSeeder,
			RequestID:        r.RequestID,
			Model:            r.Model,
			InputTokens:      r.InputTokens,
			OutputTokens:     r.OutputTokens,
			ConsumerIDHash:   hex.EncodeToString(r.ConsumerIDHash[:]),
			StartedAt:        timeStr(r.StartedAt),
			CompletedAt:      timeStr(r.CompletedAt),
			TrackerEntryHash: trackerHash,
		})
	case TransferRecord:
		var chainHash *string
		var seq *uint64
		if r.SourceChainTipHash != nil {
			s := hex.EncodeToString(r.SourceChainTipHash[:])
			chainHash = &s
			v := r.SourceSeq
			seq = &v
		}
		return json.Marshal(transferWire{
			Kind:               KindTransfer,
			RequestID:          r.RequestID,
			SourceRegion:       r.SourceRegion,
			DestRegion:         r.DestRegion,
			Amount:             r.Amount,
			Outcome:            r.Outcome,
			OutcomeReason:      r.OutcomeReason,
			SourceChainTipHash: chainHash,
			SourceSeq:          seq,
			Timestamp:          timeStr(r.Timestamp),
		})
	case UnknownRecord:
		return nil, fmt.Errorf("auditlog: cannot marshal UnknownRecord (kind=%q)", r.Kind)
	default:
		return nil, fmt.Errorf("auditlog: unknown record type %T", rec)
	}
}

// unmarshalRecord parses one JSON line and dispatches into the matching
// concrete record type. An unknown kind yields UnknownRecord (not an error).
func unmarshalRecord(line []byte) (Record, error) {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return nil, fmt.Errorf("auditlog: decode kind: %w", err)
	}
	switch head.Kind {
	case KindConsumer:
		var w consumerWire
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, fmt.Errorf("auditlog: decode consumer: %w", err)
		}
		ts, err := time.Parse(time.RFC3339Nano, w.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("auditlog: parse timestamp: %w", err)
		}
		return ConsumerRecord{
			RequestID:     w.RequestID,
			ServedLocally: w.ServedLocally,
			SeederID:      w.SeederID,
			CostCredits:   w.CostCredits,
			Timestamp:     ts,
		}, nil
	case KindTransfer:
		var w transferWire
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, fmt.Errorf("auditlog: decode transfer: %w", err)
		}
		ts, err := time.Parse(time.RFC3339Nano, w.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("auditlog: parse timestamp: %w", err)
		}
		var chainPtr *[32]byte
		if w.SourceChainTipHash != nil {
			h, err := decodeHash32(*w.SourceChainTipHash)
			if err != nil {
				return nil, fmt.Errorf("auditlog: parse source_chain_tip_hash: %w", err)
			}
			chainPtr = &h
		}
		var seq uint64
		if w.SourceSeq != nil {
			seq = *w.SourceSeq
		}
		return TransferRecord{
			RequestID:          w.RequestID,
			SourceRegion:       w.SourceRegion,
			DestRegion:         w.DestRegion,
			Amount:             w.Amount,
			Outcome:            w.Outcome,
			OutcomeReason:      w.OutcomeReason,
			SourceChainTipHash: chainPtr,
			SourceSeq:          seq,
			Timestamp:          ts,
		}, nil
	case KindSeeder:
		var w seederWire
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, fmt.Errorf("auditlog: decode seeder: %w", err)
		}
		chash, err := decodeHash32(w.ConsumerIDHash)
		if err != nil {
			return nil, fmt.Errorf("auditlog: parse consumer_id_hash: %w", err)
		}
		st, err := time.Parse(time.RFC3339Nano, w.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("auditlog: parse started_at: %w", err)
		}
		ct, err := time.Parse(time.RFC3339Nano, w.CompletedAt)
		if err != nil {
			return nil, fmt.Errorf("auditlog: parse completed_at: %w", err)
		}
		var trackerHashPtr *[32]byte
		if w.TrackerEntryHash != nil {
			h, err := decodeHash32(*w.TrackerEntryHash)
			if err != nil {
				return nil, fmt.Errorf("auditlog: parse tracker_entry_hash: %w", err)
			}
			trackerHashPtr = &h
		}
		return SeederRecord{
			RequestID:        w.RequestID,
			Model:            w.Model,
			InputTokens:      w.InputTokens,
			OutputTokens:     w.OutputTokens,
			ConsumerIDHash:   chash,
			StartedAt:        st,
			CompletedAt:      ct,
			TrackerEntryHash: trackerHashPtr,
		}, nil
	default:
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		return UnknownRecord{Kind: head.Kind, Raw: raw}, nil
	}
}

type consumerWire struct {
	Kind          string `json:"kind"`
	RequestID     string `json:"request_id"`
	ServedLocally bool   `json:"served_locally"`
	SeederID      string `json:"seeder_id,omitempty"`
	CostCredits   int64  `json:"cost_credits"`
	Timestamp     string `json:"timestamp"`
}

type seederWire struct {
	Kind             string  `json:"kind"`
	RequestID        string  `json:"request_id"`
	Model            string  `json:"model"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	ConsumerIDHash   string  `json:"consumer_id_hash"`
	StartedAt        string  `json:"started_at"`
	CompletedAt      string  `json:"completed_at"`
	TrackerEntryHash *string `json:"tracker_entry_hash,omitempty"`
}

type transferWire struct {
	Kind               string  `json:"kind"`
	RequestID          string  `json:"request_id"`
	SourceRegion       string  `json:"source_region"`
	DestRegion         string  `json:"dest_region"`
	Amount             uint64  `json:"amount"`
	Outcome            string  `json:"outcome"`
	OutcomeReason      string  `json:"outcome_reason,omitempty"`
	SourceChainTipHash *string `json:"source_chain_tip_hash,omitempty"`
	SourceSeq          *uint64 `json:"source_seq,omitempty"`
	Timestamp          string  `json:"timestamp"`
}

func timeStr(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeHash32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}
