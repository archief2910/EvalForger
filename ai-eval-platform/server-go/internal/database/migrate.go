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

// MigrateControl applies the control-plane migration history to PostgreSQL.
func MigrateControl(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return migrateDir(ctx, db, "migrations/control", "control", log)
}

// MigrateTelemetry applies the telemetry-plane migration history to DuckDB.
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
		// PostgreSQL prepares one statement per round trip, so a migration file
		// is executed statement by statement inside a single transaction.
		for _, stmt := range splitStatements(string(body)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %s/%s: %w", plane, version, err)
			}
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

// splitStatements breaks a migration file into single statements. Migration
// files in this repository contain plain DDL: no functions, no dollar quoting,
// no semicolons inside strings. Line comments are removed first because they
// may themselves contain a semicolon in prose.
func splitStatements(body string) []string {
	var out []string
	for _, raw := range strings.Split(stripLineComments(body), ";") {
		if stmt := strings.TrimSpace(raw); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func stripLineComments(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// SeedPrices copies the in-memory fallback table into model_prices so the
// control database is self-describing for tools that read it directly.
func SeedPrices(ctx context.Context, db *sql.DB) error {
	for m, p := range model.DefaultPrices {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO model_prices(model, input_per_1k, output_per_1k) VALUES (?,?,?)
			 ON CONFLICT DO NOTHING`, m, p[0], p[1]); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO model_prices(model, input_per_1k, output_per_1k) VALUES ('*default*', 0.001, 0.002)
		 ON CONFLICT DO NOTHING`)
	return err
}
