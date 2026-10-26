package database

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"evalforge/internal/model"
)

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

	res, err := tx.ExecContext(ctx, `INSERT INTO threads(
			project_id, id, status, started_at_ms, last_activity_ms, ended_at_ms,
			trace_count, created_at, updated_at)
		SELECT project_id, thread_id,
			CASE WHEN MAX(COALESCE(end_ms,start_ms)) >= ? THEN 'open' ELSE 'closed' END,
			MIN(start_ms), MAX(COALESCE(end_ms,start_ms)),
			CASE WHEN MAX(COALESCE(end_ms,start_ms)) >= ? THEN NULL ELSE MAX(COALESCE(end_ms,start_ms)) END,
			COUNT(DISTINCT trace_id), ?, ?
		FROM spans WHERE thread_id IS NOT NULL AND thread_id<>''
		GROUP BY project_id, thread_id
		ON CONFLICT DO NOTHING`, cutoff, cutoff, nowText, nowText)
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
