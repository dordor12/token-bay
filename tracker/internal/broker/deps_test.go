package broker_test

import (
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

var (
	_ broker.RegistryService  = (*registry.Registry)(nil)
	_ broker.LedgerService    = (*ledger.Ledger)(nil)
	_ broker.AdmissionService = (*admission.Subsystem)(nil)
	_ broker.IdentityResolver = (*server.Server)(nil)
)
