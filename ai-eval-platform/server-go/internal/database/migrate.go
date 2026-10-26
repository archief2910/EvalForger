package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"evalforge/internal/model"
)

//go:embed migrations/control/*.sql
//go:embed migrations/telemetry/*.sql
var migrationsFS embed.FS

// MigrateControl applies the control-plane migration history.
func MigrateControl(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return migrateDir(ctx, db, "migrations/control", "control", log)
}

// MigrateTelemetry applies the telemetry-plane migration history.
func MigrateTelemetry(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return migrateDir(ctx, db, "migrations/telemetry", "telemetry", log)
}

func migrateDir(ctx context.Context, db *sql.DB, dir, plane string, log *slog.Logger) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations (%s): %w", plane, err)
	}
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	applied := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s/%s: %w", plane, version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`,
			version, model.NowUTC()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		applied++
		log.Info("migration applied", "plane", plane, "version", version)
	}
	if applied == 0 {
		log.Info("database schema up to date", "plane", plane)
	}
	return nil
}

// SeedPrices copies the in-memory fallback table into model_prices so the
// control database is self-describing for tools that read it directly.
func SeedPrices(ctx context.Context, db *sql.DB) error {
	for m, p := range model.DefaultPrices {
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO model_prices(model, input_per_1k, output_per_1k) VALUES (?,?,?)`,
			m, p[0], p[1]); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO model_prices(model, input_per_1k, output_per_1k) VALUES ('*default*', 0.001, 0.002)`)
	return err
}

// EnsureColumn adds a column when it is missing. It exists for databases that
// predate a migration; new schema changes belong in a migration.
func EnsureColumn(ctx context.Context, db *sql.DB, table, col, typ string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil // table does not exist yet; the migration will create it
	}
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == col {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+col+" "+typ)
	return err
}
