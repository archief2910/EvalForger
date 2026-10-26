package database

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"evalforge/internal/model"
)

// MigrateLegacyTelemetry imports telemetry tables from a pre-split database.
//
// Two cases matter:
//   - sourcePath is an external LEGACY_DB or the old data/ai_eval.db: import
//     once and remember it in telemetry.plane_meta.
//   - sourcePath is the control database itself: an upgraded-in-place control
//     file may still contain spans/feedback/eval_runs. Import them, then drop
//     those legacy tables so the control plane really is metadata-only.
func MigrateLegacyTelemetry(ctx context.Context, control, telemetry *sql.DB, sourcePath, controlPath string, log *slog.Logger) error {
	if sourcePath == "" || telemetry == nil {
		return nil
	}
	if _, err := telemetry.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS plane_meta (
		key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	isControl := samePath(sourcePath, controlPath)
	if !isControl {
		if v, _ := metaValue(ctx, telemetry, "legacy_telemetry_imported"); v == "1" {
			return nil
		}
	}
	if !fileExists(sourcePath) {
		return fmt.Errorf("legacy database not found: %s", sourcePath)
	}

	src, err := sql.Open("sqlite", "file:"+sourcePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return fmt.Errorf("open legacy database %s: %w", sourcePath, err)
	}

	hasSpans, _ := tableExists(ctx, src, "spans")
	hasFeedback, _ := tableExists(ctx, src, "feedback")
	hasRuns, _ := tableExists(ctx, src, "eval_runs")
	if !hasSpans && !hasFeedback && !hasRuns {
		if !isControl {
			_ = setMeta(ctx, telemetry, "legacy_telemetry_imported", "1")
		}
		return nil
	}

	tx, err := telemetry.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	imported := map[string]int{}
	if hasSpans {
		cols := []string{"id", "trace_id", "parent_id", "name", "type", "model", "input", "output",
			"error", "metadata", "start_ms", "end_ms", "latency_ms", "prompt_tokens",
			"completion_tokens", "cost_usd", "status", "project_id", "created_at"}
		spanCols, _ := tableColumns(ctx, src, "spans")
		if spanCols["token_source"] {
			cols = append(cols, "token_source")
		}
		if spanCols["thread_id"] {
			cols = append(cols, "thread_id")
		}
		n, err := copyRowsTx(ctx, src, tx, "spans", cols)
		if err != nil {
			return fmt.Errorf("import spans: %w", err)
		}
		imported["spans"] = n
	}
	if hasFeedback {
		cols := []string{"id", "trace_id", "span_id", "kind", "value", "comment", "created_at"}
		n, err := copyRowsTx(ctx, src, tx, "feedback", cols)
		if err != nil {
			return fmt.Errorf("import feedback: %w", err)
		}
		imported["feedback"] = n
	}
	if hasRuns {
		cols := []string{"id", "experiment_id", "record_id", "input", "expected", "output",
			"error", "latency_ms", "scores", "created_at"}
		runCols, _ := tableColumns(ctx, src, "eval_runs")
		if runCols["output_mode"] {
			cols = append(cols, "output_mode")
		}
		n, err := copyRowsTx(ctx, src, tx, "eval_runs", cols)
		if err != nil {
			return fmt.Errorf("import eval runs: %w", err)
		}
		imported["eval_runs"] = n
	}
	marker := "legacy_telemetry_imported"
	if isControl {
		marker = "legacy_control_telemetry_imported"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plane_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, marker, "1"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	log.Info("telemetry imported", "source", sourcePath, "spans", imported["spans"],
		"feedback", imported["feedback"], "eval_runs", imported["eval_runs"])
	if isControl {
		if err := dropLegacyTelemetryTables(ctx, control, log); err != nil {
			return err
		}
	}
	return nil
}

// BackfillThreads gives pre-thread traces a thread id (the trace id), creates
// the matching thread rows and refreshes turn counts. It is idempotent and runs
// at every boot.
func BackfillThreads(ctx context.Context, telemetry *sql.DB, idle time.Duration, log *slog.Logger) error {
	if telemetry == nil {
		return nil
	}
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	now := time.Now().UnixMilli()
	cutoff := now - idle.Milliseconds()
	nowText := model.NowUTC()

	tx, err := telemetry.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A trace may already have an explicit thread id on one span; propagate it
	// to the rest of that trace before falling back to trace_id.
	if _, err := tx.ExecContext(ctx, `UPDATE spans SET thread_id = (
			SELECT s2.thread_id FROM spans s2
			WHERE s2.project_id=spans.project_id AND s2.trace_id=spans.trace_id
			  AND s2.thread_id IS NOT NULL AND s2.thread_id<>'' LIMIT 1)
		WHERE (thread_id IS NULL OR thread_id='')
		  AND EXISTS (SELECT 1 FROM spans s3
			WHERE s3.project_id=spans.project_id AND s3.trace_id=spans.trace_id
			  AND s3.thread_id IS NOT NULL AND s3.thread_id<>'')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE spans SET thread_id = trace_id
		WHERE thread_id IS NULL OR thread_id=''`); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO threads(
			project_id, id, status, started_at_ms, last_activity_ms, ended_at_ms,
			trace_count, created_at, updated_at)
		SELECT project_id, thread_id,
			CASE WHEN MAX(COALESCE(end_ms,start_ms)) >= ? THEN 'open' ELSE 'closed' END,
			MIN(start_ms), MAX(COALESCE(end_ms,start_ms)),
			CASE WHEN MAX(COALESCE(end_ms,start_ms)) >= ? THEN NULL ELSE MAX(COALESCE(end_ms,start_ms)) END,
			COUNT(DISTINCT trace_id), ?, ?
		FROM spans WHERE thread_id IS NOT NULL AND thread_id<>''
		GROUP BY project_id, thread_id`, cutoff, cutoff, nowText, nowText)
	if err != nil {
		return err
	}
	inserted, _ := res.RowsAffected()

	if _, err := tx.ExecContext(ctx, `UPDATE threads SET
			started_at_ms = (SELECT MIN(s.start_ms) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id),
			last_activity_ms = (SELECT MAX(COALESCE(s.end_ms,s.start_ms)) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id),
			trace_count = (SELECT COUNT(DISTINCT s.trace_id) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id)
		WHERE EXISTS (SELECT 1 FROM spans s
			WHERE s.project_id=threads.project_id AND s.thread_id=threads.id)`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if inserted > 0 {
		log.Info("threads backfilled", "created", inserted)
	}
	return nil
}

func dropLegacyTelemetryTables(ctx context.Context, control *sql.DB, log *slog.Logger) error {
	if control == nil {
		return nil
	}
	for _, table := range []string{"spans", "feedback", "eval_runs"} {
		if _, err := control.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return fmt.Errorf("drop legacy %s from control plane: %w", table, err)
		}
	}
	log.Info("control plane is metadata-only; legacy telemetry tables dropped")
	return nil
}

func copyRowsTx(ctx context.Context, src *sql.DB, tx *sql.Tx, table string, cols []string) (int, error) {
	rows, err := src.QueryContext(ctx, "SELECT "+strings.Join(cols, ",")+" FROM "+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	stmt := "INSERT OR IGNORE INTO " + table + "(" + strings.Join(cols, ",") + ") VALUES (" + placeholders + ")"
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
		if _, err := tx.ExecContext(ctx, stmt, vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	return n > 0, err
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil {
			out[name] = true
		}
	}
	return out, rows.Err()
}

func metaValue(ctx context.Context, db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM plane_meta WHERE key=?`, key).Scan(&v)
	return v, err
}

func setMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO plane_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// copySQLite makes a consistent copy of a live SQLite database. VACUUM INTO
// folds the WAL into a single file; the raw-file fallback keeps the split
// working on SQLite builds that do not expose VACUUM INTO.
func copySQLite(ctx context.Context, src, dst string) error {
	_ = os.Remove(dst)
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	quoted := strings.ReplaceAll(dst, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err == nil {
		return nil
	}
	_ = os.Remove(dst)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suffix, dst+suffix); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
