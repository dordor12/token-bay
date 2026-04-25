package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema_v1.sql
var schemaV1 string

// applyMigrations brings the database up to the latest schema. Idempotent:
// every CREATE uses IF NOT EXISTS and the schema_version row is upserted.
//
// In v1 there is exactly one migration. v2 will append additional embedded
// .sql files and run them conditionally on schema_version.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("storage: apply v1 schema: %w", err)
	}
	return nil
}
