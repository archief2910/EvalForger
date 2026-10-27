// Package retention enforces per-project telemetry retention policies.
//
// A rule says how long a project keeps its telemetry. The rule itself is
// control-plane state (small, transactional, edited by a human); the data it
// governs is telemetry-plane state (append-heavy, machine-written). Keeping
// the policy on the control side means changing how long data is kept never
// touches the append-heavy store, and the policy survives a telemetry rebuild.
//
// The sweep deletes in dependency order — human feedback and automatic scores
// first, then the spans they describe, then the conversations that no longer
// have any turns. Deleting a span without deleting its feedback would leave
// orphan rows that no view can join back to a trace.
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"evalforge/internal/model"
)

// Rule is one project's retention policy.
type Rule struct {
	ID            string
	ProjectID     string
	Name          string
	RetentionDays int
	// ApplyToPast decides whether the policy also reaches data that was
	// ingested before the rule existed. When false, only telemetry that
	// arrives from now on is expired by the rule.
	ApplyToPast bool
	Enabled     bool
	CreatedAt   string
	UpdatedAt   string
}

// Store owns retention rules and the sweeper that enforces them.
type Store struct {
	control   *sql.DB
	telemetry *sql.DB
	log       *slog.Logger

	// sweepMu serialises sweeps: two concurrent sweeps would double-count the
	// deleted rows and fight over the same telemetry pages.
	sweepMu sync.Mutex
}

// New builds a store over the two planes.
func New(control, telemetry *sql.DB, log *slog.Logger) *Store {
	return &Store{control: control, telemetry: telemetry, log: log}
}

const ruleColumns = `id, project_id, name, retention_days, apply_to_past, enabled, created_at, updated_at`

func scanRule(sc interface{ Scan(...interface{}) error }) (Rule, error) {
	var r Rule
	var applyToPast, enabled int
	err := sc.Scan(&r.ID, &r.ProjectID, &r.Name, &r.RetentionDays, &applyToPast, &enabled,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Rule{}, err
	}
	r.ApplyToPast = applyToPast != 0
	r.Enabled = enabled != 0
	return r, nil
}

// List returns every rule for a project.
func (s *Store) List(ctx context.Context, projectID string) ([]Rule, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM retention_rules WHERE project_id=? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one rule scoped to its project.
func (s *Store) Get(ctx context.Context, projectID, id string) (Rule, error) {
	row := s.control.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM retention_rules WHERE id=? AND project_id=?`, id, projectID)
	return scanRule(row)
}

// Insert stores a new rule and fills its id and timestamps.
func (s *Store) Insert(ctx context.Context, r *Rule) error {
	now := model.NowUTC()
	r.ID = model.NewID()
	r.CreatedAt, r.UpdatedAt = now, now
	_, err := s.control.ExecContext(ctx,
		`INSERT INTO retention_rules(id, project_id, name, retention_days, apply_to_past, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.ProjectID, r.Name, r.RetentionDays, boolInt(r.ApplyToPast), boolInt(r.Enabled),
		r.CreatedAt, r.UpdatedAt)
	return err
}

// Save updates the mutable fields of a rule.
func (s *Store) Save(ctx context.Context, r Rule) error {
	r.UpdatedAt = model.NowUTC()
	_, err := s.control.ExecContext(ctx,
		`UPDATE retention_rules SET name=?, retention_days=?, apply_to_past=?, enabled=?, updated_at=?
		 WHERE id=? AND project_id=?`,
		r.Name, r.RetentionDays, boolInt(r.ApplyToPast), boolInt(r.Enabled), r.UpdatedAt, r.ID, r.ProjectID)
	return err
}

// Delete removes a rule. The telemetry it governed is not touched: a deleted
// policy stops expiring data, it does not delete it.
func (s *Store) Delete(ctx context.Context, projectID, id string) error {
	_, err := s.control.ExecContext(ctx,
		`DELETE FROM retention_rules WHERE id=? AND project_id=?`, id, projectID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- enforcement ----------

// Result reports what one sweep removed.
type Result struct {
	RuleID         string `json:"rule_id"`
	ProjectID      string `json:"project_id"`
	RetentionDays  int    `json:"retention_days"`
	SpansDeleted   int    `json:"spans_deleted"`
	FeedbackGone   int    `json:"feedback_deleted"`
	ScoresGone     int    `json:"online_scores_deleted"`
	ThreadsDeleted int    `json:"threads_deleted"`
	ThreadScores   int    `json:"thread_scores_deleted"`
}

// Sweep enforces every enabled rule. It returns one Result per rule that did
// work, so a caller (or a test) can see which policy removed what.
func (s *Store) Sweep(ctx context.Context) ([]Result, error) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	rows, err := s.control.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM retention_rules WHERE enabled=1 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		rules = append(rules, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []Result{}
	for _, r := range rules {
		res, err := s.sweepRule(ctx, r)
		if err != nil {
			return out, err
		}
		if res.SpansDeleted > 0 || res.ThreadsDeleted > 0 {
			out = append(out, res)
		}
	}
	return out, nil
}

// Apply runs one rule immediately and returns what it removed. It is what the
// "apply now" endpoint calls, so someone who has just set a policy on a
// project with a lot of data does not have to wait for the schedule.
func (s *Store) Apply(ctx context.Context, r Rule) (Result, error) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	return s.sweepRule(ctx, r)
}

// sweepRule applies one rule to the telemetry plane.
func (s *Store) sweepRule(ctx context.Context, r Rule) (Result, error) {
	res := Result{RuleID: r.ID, ProjectID: r.ProjectID, RetentionDays: r.RetentionDays}
	if r.RetentionDays <= 0 {
		return res, nil
	}
	cutoff := time.Now().Add(-time.Duration(r.RetentionDays) * 24 * time.Hour).UnixMilli()

	// A rule that does not apply to the past must not delete data that was
	// already there when it was created, so the lower bound is the rule's
	// creation time rather than zero.
	lowerBound := int64(0)
	if !r.ApplyToPast {
		if created, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
			lowerBound = created.UnixMilli()
		}
	}

	tx, err := s.telemetry.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// The expired traces are selected once, before anything is deleted, into a
	// temporary table. The feedback table has no project column, so the
	// project scope has to come from the spans that describe the trace — and
	// that link is only intact while the spans still exist. A temp table keeps
	// the selection stable across the deletes without pulling a potentially
	// huge list of ids into Go memory.
	if _, err := tx.ExecContext(ctx, `CREATE TEMPORARY TABLE IF NOT EXISTS retention_expired_traces (
		project_id TEXT NOT NULL, trace_id TEXT NOT NULL)`); err != nil {
		return res, fmt.Errorf("retention: create temp table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM retention_expired_traces`); err != nil {
		return res, fmt.Errorf("retention: clear temp table: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO retention_expired_traces(project_id, trace_id)
		 SELECT DISTINCT project_id, trace_id FROM spans
		 WHERE project_id=? AND start_ms < ? AND start_ms >= ?`,
		r.ProjectID, cutoff, lowerBound); err != nil {
		return res, fmt.Errorf("retention: select expired traces: %w", err)
	}

	// Delete children before parents. Feedback and automatic scores reference
	// traces, so they go first; the spans they describe go last.
	if n, err := execCount(ctx, tx,
		`DELETE FROM feedback WHERE trace_id IN (
			SELECT trace_id FROM retention_expired_traces WHERE project_id=?)`, r.ProjectID); err != nil {
		return res, err
	} else {
		res.FeedbackGone += n
	}
	if n, err := execCount(ctx, tx,
		`DELETE FROM online_scores WHERE trace_id IN (
			SELECT trace_id FROM retention_expired_traces WHERE project_id=?)`, r.ProjectID); err != nil {
		return res, err
	} else {
		res.ScoresGone += n
	}
	if n, err := execCount(ctx, tx,
		`DELETE FROM spans WHERE project_id=? AND trace_id IN (
			SELECT trace_id FROM retention_expired_traces WHERE project_id=?)`,
		r.ProjectID, r.ProjectID); err != nil {
		return res, err
	} else {
		res.SpansDeleted += n
	}

	// Conversations whose turns are all gone go too, and their scores with
	// them, otherwise the conversation list fills up with empty ghosts. The
	// threads go first, so the score delete can look for the parent that no
	// longer exists.
	if n, err := execCount(ctx, tx,
		`DELETE FROM threads WHERE project_id=? AND NOT EXISTS (
			SELECT 1 FROM spans s WHERE s.project_id=threads.project_id AND s.thread_id=threads.id)`,
		r.ProjectID); err != nil {
		return res, err
	} else {
		res.ThreadsDeleted += n
	}
	if n, err := execCount(ctx, tx,
		`DELETE FROM thread_scores WHERE project_id=? AND NOT EXISTS (
			SELECT 1 FROM threads t WHERE t.project_id=thread_scores.project_id AND t.id=thread_scores.thread_id)`,
		r.ProjectID); err != nil {
		return res, err
	} else {
		res.ThreadScores += n
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// execCount runs a delete and returns how many rows it removed.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...interface{}) (int, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("retention delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// DuckDB and PostgreSQL both report the count; a driver that does not
		// is not a failure, the delete still happened.
		return 0, nil
	}
	return int(n), nil
}
