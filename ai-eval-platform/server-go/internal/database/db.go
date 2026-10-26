// Package database owns the two SQLite connections and the forward-only
// migration history. It is the only package that knows how storage is opened.
//
// The split is deliberate:
//
//	control.db   small, relational, transactional metadata
//	telemetry.db append-heavy traces, spans, feedback and eval results
//
// They are separate files, so they can be backed up, vacuumed, retained and
// scaled independently. Handlers never join across the two files; they read
// each plane and merge in Go.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"evalforge/internal/config"

	_ "modernc.org/sqlite"
)

// Stores is the pair of database handles owned by the server.
type Stores struct {
	Control   *sql.DB
	Telemetry *sql.DB
}

// Open opens only the control-plane database. Tests and small tools that need
// metadata can use it; the server uses OpenStores.
func Open(cfg config.Config) (*sql.DB, error) {
	path := ControlPathFor(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return openFile(path)
}

// OpenTelemetry opens only the telemetry-plane database.
func OpenTelemetry(cfg config.Config) (*sql.DB, error) {
	path := TelemetryPathFor(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return openFile(path)
}

// OpenStores opens both planes, applies their independent migrations, adopts a
// legacy single-file database when one exists, and backfills threads for traces
// that predate first-class conversations.
func OpenStores(ctx context.Context, cfg config.Config, log *slog.Logger) (*Stores, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	controlPath := ControlPathFor(cfg)
	telemetryPath := TelemetryPathFor(cfg)
	if samePath(controlPath, telemetryPath) {
		return nil, fmt.Errorf("control and telemetry databases must be different files")
	}

	// One-time adoption of the old single-file layout. Copy (never move) so a
	// failed split leaves the original database intact.
	legacyDefault := filepath.Join(cfg.DataDir, "ai_eval.db")
	if !fileExists(controlPath) && fileExists(legacyDefault) && !samePath(controlPath, legacyDefault) {
		if err := copySQLite(ctx, legacyDefault, controlPath); err != nil {
			return nil, fmt.Errorf("copy legacy database to control plane: %w", err)
		}
		log.Info("adopted legacy database as control plane", "from", legacyDefault, "to", controlPath)
	}

	control, err := openFile(controlPath)
	if err != nil {
		return nil, fmt.Errorf("open control plane: %w", err)
	}
	telemetry, err := openFile(telemetryPath)
	if err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("open telemetry plane: %w", err)
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

	// Import an explicitly configured older database first, then any telemetry
	// tables still sitting in the control file (upgraded-in-place layout).
	legacyPath := cfg.LegacyDB
	if legacyPath == "" && fileExists(legacyDefault) {
		legacyPath = legacyDefault
	}
	if legacyPath != "" {
		if err := MigrateLegacyTelemetry(ctx, control, telemetry, legacyPath, controlPath, log); err != nil {
			closeBoth()
			return nil, fmt.Errorf("migrate legacy telemetry: %w", err)
		}
	}
	if err := MigrateLegacyTelemetry(ctx, control, telemetry, controlPath, controlPath, log); err != nil {
		closeBoth()
		return nil, fmt.Errorf("split telemetry out of control plane: %w", err)
	}
	if err := BackfillThreads(ctx, telemetry, cfg.ThreadIdle, log); err != nil {
		closeBoth()
		return nil, fmt.Errorf("backfill threads: %w", err)
	}

	return &Stores{Control: control, Telemetry: telemetry}, nil
}

func openFile(path string) (*sql.DB, error) {
	dsn := "file:" + path +
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
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	return db, nil
}

// ControlPathFor resolves the control-plane file. DBPath is kept as a
// compatibility alias for callers that predate the split.
func ControlPathFor(cfg config.Config) string {
	if cfg.ControlDBPath != "" {
		return cfg.ControlDBPath
	}
	if cfg.DBPath != "" {
		return cfg.DBPath
	}
	if cfg.DataDir != "" {
		return filepath.Join(cfg.DataDir, "control.db")
	}
	return "control.db"
}

// TelemetryPathFor resolves the telemetry-plane file.
func TelemetryPathFor(cfg config.Config) string {
	if cfg.TelemetryDBPath != "" {
		return cfg.TelemetryDBPath
	}
	if cfg.DataDir != "" {
		return filepath.Join(cfg.DataDir, "telemetry.db")
	}
	return "telemetry.db"
}

// Path is the historical helper name for the control-plane path.
func Path(dir string) string { return filepath.Join(dir, "control.db") }

// TelemetryPath is the telemetry-plane path inside a data directory.
func TelemetryPath(dir string) string { return filepath.Join(dir, "telemetry.db") }

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(aa, bb)
}
