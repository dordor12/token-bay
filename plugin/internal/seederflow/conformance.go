package seederflow

import (
	"context"
	"fmt"
	"time"
)

// RunConformance executes the configured ConformanceFn against the
// configured Runner. On failure, the coordinator marks itself as
// non-advertising until ResetConformance is called. Plugin spec §11
// requires this gate at boot and on every Claude Code version change.
//
// A nil ConformanceFn is a no-op: tests and early bring-up may skip
// the check. The cmd layer wires ccbridge.RunStartupConformance in
// production.
func (c *Coordinator) RunConformance(ctx context.Context) error {
	if c.cfg.ConformanceFn == nil {
		return nil
	}
	if err := c.cfg.ConformanceFn(ctx, c.cfg.Runner); err != nil {
		c.mu.Lock()
		c.conformanceFailed = true
		c.mu.Unlock()
		return fmt.Errorf("seederflow: conformance: %w", err)
	}
	c.mu.Lock()
	c.conformanceFailed = false
	c.mu.Unlock()
	return nil
}

// ResetConformance clears the conformance-failed flag. The cmd layer
// calls this after a successful re-run when the operator updates the
// Claude Code binary.
func (c *Coordinator) ResetConformance() {
	c.mu.Lock()
	c.conformanceFailed = false
	c.mu.Unlock()
}

// Now returns the coordinator's clock reading. Useful in tests that
// want to ask Available without recreating time.Now elsewhere.
func (c *Coordinator) Now() time.Time { return c.cfg.Clock() }
