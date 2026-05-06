package api_test

import (
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// Compile-time assertions that the concrete tracker subsystems satisfy
// the unions declared by api. Catches signature drift between
// internal/api's narrow interfaces and the real subsystem code.
var (
	_ api.LedgerService   = (*ledger.Ledger)(nil)
	_ api.RegistryService = (*registry.Registry)(nil)
)

// Note: api.StunTurnService is only satisfied by the cmd-side adapter,
// not by *stunturn.Allocator alone (Reflect lives at package level, not
// on the type). cmd/run_cmd_test asserts that wiring.
