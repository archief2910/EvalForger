package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"evalforge/internal/config"
	"evalforge/internal/model"
)

// The tables each plane owns. Importing is deliberately whitelisted: a legacy
// file may contain anything, and only these tables have a home in the new
// schema.
var controlTables = []string{
	"projects", "datasets", "dataset_records", "scorers", "experiments",
	"prompts", "model_prices", "jobs", "api_keys", "oauth_clients",
	"oauth_codes", "oauth_tokens", "dataset_versions", "feedback_definitions",
}

var telemetryTables = []string{
	"spans", "feedback", "eval_runs", "threads", "thread_scores",
}

// ImportLegacySQLite moves pre-existing SQLite data into the two new engines.
//
// It is a one-time, idempotent transport: every source file is imported in full
// once, recorded in control.import_state, and never read again. Rows are
// inserted with ON CONFLICT DO NOTHING, so a partially failed run can simply be
// retried and existing rows always win. The SQLite files are opened read-only
// and left on disk.
func ImportLegacySQLite(ctx context.Context, control, telemetry *sql.DB, cfg config.Config, log *slog.Logger) error {
	for _, path := range legacyPaths(cfg) {
		if err := importFile(ctx, control, telemetry, path, log); err != nil {
			return err
		}
	}
	return nil
}

// legacyPaths returns the SQLite files worth importing, oldest first. An
// explicit LEGACY_DB wins; otherwise the conventional files in DATA_DIR are
// picked up if they exist. The old single-file database is imported before the
// split control file so the newer rows win their conflicts.
func legacyPaths(cfg config.Config) []string {
	var candidates []string
	if cfg.LegacyDB != "" {
		candidates = append(candidates, cfg.LegacyDB)
	}
	if cfg.DataDir != "" {
		candidates = append(candidates,
			filepath.Join(cfg.DataDir, "ai_eval.db"),
			filepath.Join(cfg.DataDir, "telemetry.db"),
			filepath.Join(cfg.DataDir, "control.db"),
		)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			continue
		}
		// Never import the DuckDB file back into itself.
		if strings.EqualFold(abs, mustAbs(cfg.TelemetryPath)) {
			continue
		}
		out = append(out, abs)
	}
	return out
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func importFile(ctx context.Context, control, telemetry *sql.DB, path string, log *slog.Logger) error {
	var already int
	if err := control.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM import_state WHERE source=?`, path).Scan(&already); err != nil {
		return fmt.Errorf("read import state: %w", err)
	}
	if already > 0 {
		return nil
	}

	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open legacy sqlite %s: %w", path, err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return fmt.Errorf("open legacy sqlite %s: %w", path, err)
	}

	source, err := sourceTables(ctx, src)
	if err != nil {
		return fmt.Errorf("read schema of %s: %w", path, err)
	}

	counts := map[string]int{}
	for _, table := range controlTables {
		if !source[table] {
			continue
		}
		n, err := copyTable(ctx, src, control, table, "postgres")
		if err != nil {
			return fmt.Errorf("import %s.%s: %w", filepath.Base(path), table, err)
		}
		counts["control."+table] = n
	}
	for _, table := range telemetryTables {
		if !source[table] {
			continue
		}
		n, err := copyTable(ctx, src, telemetry, table, "duckdb")
		if err != nil {
			return fmt.Errorf("import %s.%s: %w", filepath.Base(path), table, err)
		}
		counts["telemetry."+table] = n
	}

	payload, _ := json.Marshal(counts)
	if _, err := control.ExecContext(ctx,
		`INSERT INTO import_state(source, imported_at, rows_json) VALUES (?,?,?)
		 ON CONFLICT DO NOTHING`, path, model.NowUTC(), string(payload)); err != nil {
		return fmt.Errorf("record import of %s: %w", path, err)
	}
	log.Info("legacy sqlite imported", "source", path, "tables", len(counts), "rows", totalRows(counts))
	return nil
}

func totalRows(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

func sourceTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// copyTable copies the columns both sides share, row by row, into a single
// transaction. Unknown target columns take their defaults.
func copyTable(ctx context.Context, src, dst *sql.DB, table, dialect string) (int, error) {
	targetCols, err := targetColumns(ctx, dst, table, dialect)
	if err != nil {
		return 0, err
	}
	sourceCols, err := sourceColumns(ctx, src, table)
	if err != nil {
		return 0, err
	}
	var cols []string
	for _, c := range sourceCols {
		if targetCols[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return 0, nil
	}

	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
	}
	selectSQL := `SELECT ` + strings.Join(quoted, ", ") + ` FROM "` + table + `"`
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	insertSQL := `INSERT INTO "` + table + `"(` + strings.Join(quoted, ", ") + `) VALUES (` + placeholders + `)
		ON CONFLICT DO NOTHING`

	rows, err := src.QueryContext(ctx, selectSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	n := 0
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, v := range values {
			// SQLite has no blob columns here, but the driver can still hand
			// back []byte for text; both engines would reject that as bytea.
			if b, ok := v.([]byte); ok {
				values[i] = string(b)
			}
		}
		if _, err := tx.ExecContext(ctx, insertSQL, values...); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

func sourceColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type col struct {
		cid     int
		name    string
		ctype   string
		notnull int
		dflt    interface{}
		pk      int
	}
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.cid, &c.name, &c.ctype, &c.notnull, &c.dflt, &c.pk); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].cid < cols[j].cid })
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out, nil
}

func targetColumns(ctx context.Context, db *sql.DB, table, dialect string) (map[string]bool, error) {
	var query string
	if dialect == "postgres" {
		query = `SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ?`
	} else {
		query = `SELECT column_name FROM information_schema.columns
			WHERE table_catalog = current_database() AND table_schema = 'main' AND table_name = ?`
	}
	rows, err := db.QueryContext(ctx, query, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
