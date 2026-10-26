// Package database owns the two storage engines and their forward-only
// migration histories. It is the only package that knows how storage is opened.
//
// The split is deliberate and follows the shape of the product it models:
//
//	control plane  PostgreSQL  small, relational, transactional metadata
//	telemetry plane DuckDB     append-heavy traces, spans, feedback, eval results
//
// They are separate engines with separate connections, migrations, backup and
// retention paths. Handlers never join across them; they read each plane and
// merge the results in Go.
//
// PostgreSQL is reached through the pgbind driver, which lets the statements
// keep using `?` placeholders (see internal/pgbind). DuckDB speaks `?`
// natively, so a single spelling works against both engines.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/marcboeker/go-duckdb/v2"

	"evalforge/internal/config"
	"evalforge/internal/pgbind"
)

// Stores is the pair of database handles owned by the server.
type Stores struct {
	Control   *sql.DB
	Telemetry *sql.DB
}

// OpenStores connects to both planes, applies their independent migrations,
// imports any pre-existing SQLite database once, and backfills conversation
// threads for traces that predate them.
func OpenStores(ctx context.Context, cfg config.Config, log *slog.Logger) (*Stores, error) {
	if err := ensureSchema(ctx, cfg); err != nil {
		return nil, err
	}

	control := pgbind.Open(controlDSN(cfg))
	control.SetMaxOpenConns(10)
	control.SetMaxIdleConns(10)
	control.SetConnMaxIdleTime(5 * time.Minute)
	if err := ping(ctx, control, "control", cfg.ControlURL); err != nil {
		_ = control.Close()
		return nil, err
	}

	telemetry, err := openTelemetry(cfg.TelemetryPath)
	if err != nil {
		_ = control.Close()
		return nil, err
	}
	closeBoth := func() { _ = control.Close(); _ = telemetry.Close() }

	if err := MigrateControl(ctx, control, log); err != nil {
		closeBoth()
		return nil, err
	}
	if err := MigrateTelemetry(ctx, telemetry, log); err != nil {
		closeBoth()
		return nil, err
	}
	if err := SeedPrices(ctx, control); err != nil {
		closeBoth()
		return nil, fmt.Errorf("seed model prices: %w", err)
	}

	if err := ImportLegacySQLite(ctx, control, telemetry, cfg, log); err != nil {
		closeBoth()
		return nil, fmt.Errorf("import legacy sqlite: %w", err)
	}
	if err := BackfillThreads(ctx, telemetry, cfg.ThreadIdle, log); err != nil {
		closeBoth()
		return nil, fmt.Errorf("backfill threads: %w", err)
	}

	return &Stores{Control: control, Telemetry: telemetry}, nil
}

// controlDSN pins the schema for this process. Tests and harnesses give every
// run its own schema, so they cannot see each other's rows.
func controlDSN(cfg config.Config) string {
	u, err := url.Parse(cfg.ControlURL)
	if err != nil {
		return cfg.ControlURL
	}
	if cfg.ControlSchema != "" && cfg.ControlSchema != "public" {
		q := u.Query()
		q.Set("search_path", cfg.ControlSchema)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ensureSchema creates a non-default schema before anything connects with a
// search_path that expects it to exist.
func ensureSchema(ctx context.Context, cfg config.Config) error {
	if cfg.ControlSchema == "" || cfg.ControlSchema == "public" {
		return nil
	}
	db := pgbind.Open(cfg.ControlURL)
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS "`+cfg.ControlSchema+`"`); err != nil {
		return fmt.Errorf("create schema %s: %w", cfg.ControlSchema, err)
	}
	return nil
}

// DropSchema removes a schema and everything in it. Tests call it in cleanup;
// it is never used by the server.
func DropSchema(ctx context.Context, cfg config.Config) error {
	if cfg.ControlSchema == "" || cfg.ControlSchema == "public" {
		return fmt.Errorf("refusing to drop the default schema")
	}
	db := pgbind.Open(cfg.ControlURL)
	defer db.Close()
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+cfg.ControlSchema+`" CASCADE`)
	return err
}

func openTelemetry(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create telemetry dir: %w", err)
		}
	}
	// A connector keeps every pooled connection on one DuckDB instance. DuckDB
	// allows a single writer process per file, so the server must be the only
	// process opening this path.
	connector, err := duckdb.NewConnector(path, nil)
	if err != nil {
		return nil, fmt.Errorf("open telemetry %s: %w", path, err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := ping(context.Background(), db, "telemetry", path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ping(ctx context.Context, db *sql.DB, plane, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect %s plane at %s: %w", plane, target, err)
	}
	return nil
}
