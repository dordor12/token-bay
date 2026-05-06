//go:build windows

package ccbridge

import (
	"context"
	"io"
)

// Run implements Runner on Windows. The seeder bridge requires Unix
// process-group semantics for clean kill-on-cancel; Windows support
// is deferred per plugin spec §10.
func (r *ExecRunner) Run(_ context.Context, _ Request, _ io.Writer) error {
	return ErrUnsupportedPlatform
}
