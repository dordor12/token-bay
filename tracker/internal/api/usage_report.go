package api

// installUsageReport is currently always a Scope-2 stub. Replaced when
// broker lands and routes seeder-signed usage reports to ledger.
func (r *Router) installUsageReport() handlerFunc { return notImpl("usage_report") }
