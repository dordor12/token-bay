package api

// installSettle is currently always a Scope-2 stub. The future broker
// PR replaces this body with a real handler that calls
// ledger.AppendUsage on the dispatched preimage. Until then it returns
// ErrNotImplemented regardless of Deps wiring.
func (r *Router) installSettle() handlerFunc { return notImpl("settle") }
