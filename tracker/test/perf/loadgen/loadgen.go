//go:build perf

// Package loadgen hosts the simulated Token-Bay participants of the
// tracker performance harness (spec 2026-07-12 §3): goroutine-scale
// consumers and seeders that speak the real QUIC wire protocol against
// live tracker containers, but skip the consumer↔seeder tunnel — the
// tracker is control-path only, so a seeder that accepts an offer and
// reports usage exercises every tracker code path load cares about.
//
// Reused plumbing: tracker/test/e2e/driver.RPCClient (mTLS QUIC, frame
// IO, push-stream accept, heartbeats), shared/signing, and the
// broker's own PriceTable so reported costs can never drift from what
// the tracker recomputes at settlement.
package loadgen

import (
	"encoding/hex"
	"sync"
	"sync/atomic"

	"github.com/token-bay/token-bay/tracker/test/perf/report"
)

// Counters aggregates client-side observations across every simulated
// participant. All fields are safe for concurrent use.
type Counters struct {
	Assigned        atomic.Int64
	NoCapacity      atomic.Int64
	Queued          atomic.Int64
	Rejected        atomic.Int64
	BudgetExhausted atomic.Int64

	// TotalOps / ErrorOps cover meaningful client operations: dials,
	// ENROLL, ADVERTISE, BALANCE, BROKER_REQUEST, USAGE_REPORT, SETTLE.
	// Heartbeats are excluded — a dead connection already surfaces
	// through the operations above, and thousands of successful pings
	// would dilute the error-rate threshold into meaninglessness.
	TotalOps atomic.Int64
	ErrorOps atomic.Int64

	UsageReportsSent  atomic.Int64
	SettlementsSigned atomic.Int64

	BrokerLatency report.Histogram

	mu         sync.Mutex
	seenTokens map[string]struct{}
	dupTokens  int64
}

// Op records one attempted client operation; failed reports whether it
// errored.
func (c *Counters) Op(failed bool) {
	c.TotalOps.Add(1)
	if failed {
		c.ErrorOps.Add(1)
	}
}

// RecordAssignment tracks a reservation token and flags duplicates —
// the same token handed to two assignments is a broker double-assign.
func (c *Counters) RecordAssignment(token []byte) {
	key := hex.EncodeToString(token)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seenTokens == nil {
		c.seenTokens = make(map[string]struct{})
	}
	if _, dup := c.seenTokens[key]; dup {
		c.dupTokens++
		return
	}
	c.seenTokens[key] = struct{}{}
}

// DuplicateAssignments returns the number of duplicate reservation
// tokens observed.
func (c *Counters) DuplicateAssignments() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dupTokens
}

// Snapshot folds the counters into the report summary's client-side
// fields.
func (c *Counters) Snapshot(s *report.Summary) {
	s.Outcomes = report.Outcomes{
		Assigned:        c.Assigned.Load(),
		NoCapacity:      c.NoCapacity.Load(),
		Queued:          c.Queued.Load(),
		Rejected:        c.Rejected.Load(),
		BudgetExhausted: c.BudgetExhausted.Load(),
		Errors:          c.ErrorOps.Load(),
	}
	s.TotalOps = c.TotalOps.Load()
	s.ErrorOps = c.ErrorOps.Load()
	s.BrokerP50 = c.BrokerLatency.Percentile(0.50)
	s.BrokerP95 = c.BrokerLatency.Percentile(0.95)
	s.BrokerP99 = c.BrokerLatency.Percentile(0.99)
	s.UsageReportsSent = c.UsageReportsSent.Load()
	s.SettlementsSigned = c.SettlementsSigned.Load()
	s.DuplicateAssignments = c.DuplicateAssignments()
}
