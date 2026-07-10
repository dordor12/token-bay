//go:build e2e

package driver

// SQLiteQuery runs `sqlite3 <dbPath> "<sql>"` inside the given compose
// service via `docker compose exec -T <service> sqlite3 <dbPath> <sql>`.
// Used by ledger-integrity scenarios to inspect the tracker's on-disk
// ledger/registry SQLite files directly (e.g. asserting a USAGE entry
// row has both consumer_sig and seeder_sig populated).
//
// Requires the sqlite3 CLI inside the tracker image — the orchestrator
// adds `sqlite` to the runtime `apk add` in
// tracker/deployments/docker/Dockerfile (Task 23 Dockerfile addendum);
// this helper only shells out to the binary, it does not install it.
func SQLiteQuery(c Compose, service, dbPath, sql string) (string, error) {
	return c.Exec(service, "sqlite3", dbPath, sql)
}
