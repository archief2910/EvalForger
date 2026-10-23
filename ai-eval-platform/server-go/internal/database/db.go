// Package database owns the SQLite connection and the forward-only migration
// history. It is the only package that knows how the database is opened.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"evalforge/internal/config"
	"evalforge/internal/model"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open creates the data directory, opens SQLite in WAL mode and returns a pool
// sized for concurrent readers with explicit write serialisation by callers.
func Open(cfg config.Config) (*sql.DB, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + cfg.DBPath +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

// SeedPrices copies the in-memory fallback table into model_prices so the
// database is self-describing for tools that read it directly.
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

// Migrate applies every embedded migration that has not been recorded in
// schema_migrations. Each migration runs in a transaction, so a failure leaves
// the database at the previous version instead of half-migrated.
func Migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
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
		body, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", version, err)
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
		log.Info("migration applied", "version", version)
	}
	// Compatibility for databases created before migrations existed. New
	// databases get this column from 0001; old ones get it added here.
	if err := EnsureColumn(ctx, db, "eval_runs", "output_mode", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if applied == 0 {
		log.Info("database schema up to date")
	}
	return nil
}

// EnsureColumn adds a column when it is missing. It exists for databases that
// predate the migration history; new schema changes belong in a migration.
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

// MigrateLegacy copies rows from an older SQLite file when LEGACY_DB is set.
// The insert is INSERT OR IGNORE, so running it twice is harmless.
func MigrateLegacy(ctx context.Context, db *sql.DB, legacyPath string, log *slog.Logger) error {
	if legacyPath == "" {
		return nil
	}
	log.Info("migrating legacy database", "path", legacyPath)
	ldb, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		return fmt.Errorf("open legacy: %w", err)
	}
	defer ldb.Close()

	tables := []struct{ table, cols string }{
		{"projects", "id,name,description,created_at"},
		{"spans", "id,trace_id,parent_id,name,type,model,input,output,error,metadata,start_ms,end_ms,latency_ms,prompt_tokens,completion_tokens,cost_usd,status,project_id,created_at"},
		{"feedback", "id,trace_id,span_id,kind,value,comment,created_at"},
		{"datasets", "id,project_id,name,description,version,created_at"},
		{"dataset_records", "id,dataset_id,input,expected,metadata,tags,source,created_at"},
		{"experiments", "id,project_id,name,dataset_id,baseline_experiment_id,status,summary,created_at"},
		{"eval_runs", "id,experiment_id,record_id,input,expected,output,error,latency_ms,scores,output_mode,created_at"},
		{"prompts", "id,project_id,name,version,model,template,params,created_at"},
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tables {
		rows, err := ldb.Query("SELECT " + t.cols + " FROM " + t.table)
		if err != nil {
			log.Warn("legacy table skipped", "table", t.table, "err", err)
			continue
		}
		cols := strings.Split(t.cols, ",")
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
		n := 0
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO "+t.table+"("+t.cols+") VALUES ("+placeholders+")", vals...); err != nil {
				log.Warn("legacy row skipped", "table", t.table, "err", err)
				continue
			}
			n++
		}
		rows.Close()
		log.Info("legacy table migrated", "table", t.table, "rows", n)
	}
	return tx.Commit()
}

// Path is a small helper for logs and tests.
func Path(dir string) string { return filepath.Join(dir, "ai_eval.db") }
