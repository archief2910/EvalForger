// Package queue implements the durable evaluation job queue: claim, lease,
// heartbeat, retry and boot requeue. The UPDATE is the lock, so two dispatchers
// cannot claim the same job even if they share a database file.
package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"evalforge/internal/model"
)

const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Job is one claimable unit of work.
type Job struct {
	ID           string
	ExperimentID string
	ProjectID    string
	Payload      string
	Attempts     int
	MaxAttempts  int
}

// Outcome is what a handler reports. The queue owns status transitions.
type Outcome struct {
	Diagnostic string
	TimedOut   bool
	// Released means shutdown interrupted the work: put it back in the queue
	// without consuming an attempt.
	Released bool
}

// Handler executes one job. It must be safe to call again after a retry.
type Handler func(ctx context.Context, j Job) Outcome

// Queue is a durable, single-writer job dispatcher.
type Queue struct {
	db     *sql.DB
	mu     *sync.Mutex
	log    *slog.Logger
	lease  time.Duration
	run    Handler
	retire func(expID, reason string)
}

// New wires a queue. mu must be the same write mutex the API uses so SQLite
// sees one writer at a time.
func New(db *sql.DB, mu *sync.Mutex, log *slog.Logger, lease time.Duration, run Handler, retire func(expID, reason string)) *Queue {
	return &Queue{db: db, mu: mu, log: log, lease: lease, run: run, retire: retire}
}

// EnqueueEvalTx writes the job in the same transaction as its experiment, so a
// crash cannot leave an experiment with no owner.
func (q *Queue) EnqueueEvalTx(ctx context.Context, tx *sql.Tx, expID, projectID string, payload []byte) error {
	ts := model.NowUTC()
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs(id, kind, experiment_id, project_id, payload, status,
		attempts, max_attempts, created_at, updated_at)
		VALUES (?,?,?,?,?,?,0,2,?,?)`,
		model.NowUTC(), "eval", expID, projectID, string(payload), StatusQueued, ts, ts)
	return err
}

// Start owns the dispatcher loop and its reaper ticker.
func (q *Queue) Start(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		lastReap := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if time.Since(lastReap) > 10*time.Second {
				if n := q.ReapExpired(); n > 0 {
					q.log.Warn("requeued expired job leases", "count", n)
				}
				lastReap = time.Now()
			}
			j, err := q.claim(time.Now())
			if err != nil {
				q.log.Error("claim job", "err", err)
				continue
			}
			if j == nil {
				continue
			}
			q.execute(ctx, j)
		}
	}()
	return &wg
}

func (q *Queue) execute(ctx context.Context, j *Job) {
	hbCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		t := time.NewTicker(q.heartbeat())
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				q.renewLease(j.ID)
			}
		}
	}()

	out := q.run(ctx, *j)
	stop()

	switch {
	case out.Diagnostic == "" && !out.TimedOut && !out.Released:
		q.mark(j.ID, StatusDone, "")
		q.log.Info("job done", "job_id", j.ID, "experiment_id", j.ExperimentID)

	case out.Released || errors.Is(ctx.Err(), context.Canceled):
		q.mark(j.ID, StatusQueued, "released on shutdown")
		q.log.Info("job released for restart", "job_id", j.ID)

	case out.TimedOut:
		q.mark(j.ID, StatusFailed, out.Diagnostic)
		q.retire(j.ExperimentID, out.Diagnostic)

	case j.Attempts < j.MaxAttempts:
		q.mark(j.ID, StatusQueued, out.Diagnostic)
		q.log.Warn("job will retry", "job_id", j.ID, "attempt", j.Attempts, "max", j.MaxAttempts, "err", trimForLog(out.Diagnostic))

	default:
		q.mark(j.ID, StatusFailed, out.Diagnostic)
		q.retire(j.ExperimentID, out.Diagnostic)
		q.log.Error("job failed", "job_id", j.ID, "attempts", j.Attempts, "err", trimForLog(out.Diagnostic))
	}
}

// claim picks the oldest queued job or one whose lease expired.
func (q *Queue) claim(now time.Time) (*Job, error) {
	ts := now.UTC().Format(time.RFC3339Nano)
	lease := now.Add(q.lease).UTC().Format(time.RFC3339Nano)

	q.mu.Lock()
	defer q.mu.Unlock()

	var id string
	err := q.db.QueryRow(`SELECT id FROM jobs
		WHERE status=? OR (status=? AND (lease_expires_at IS NULL OR lease_expires_at < ?))
		ORDER BY created_at LIMIT 1`, StatusQueued, StatusRunning, ts).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	res, err := q.db.Exec(`UPDATE jobs SET status=?, attempts=attempts+1, lease_expires_at=?, updated_at=?
		WHERE id=? AND (status=? OR lease_expires_at < ?)`,
		StatusRunning, lease, ts, id, StatusQueued, ts)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}

	var j Job
	err = q.db.QueryRow(`SELECT id, experiment_id, project_id, payload, attempts, max_attempts
		FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.ExperimentID, &j.ProjectID, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (q *Queue) renewLease(jobID string) {
	until := time.Now().Add(q.lease).UTC().Format(time.RFC3339Nano)
	q.mu.Lock()
	_, _ = q.db.Exec(`UPDATE jobs SET lease_expires_at=?, updated_at=? WHERE id=?`, until, model.NowUTC(), jobID)
	q.mu.Unlock()
}

func (q *Queue) mark(jobID, status, errMsg string) {
	q.mu.Lock()
	_, _ = q.db.Exec(`UPDATE jobs SET status=?, last_error=?, lease_expires_at=NULL, updated_at=? WHERE id=?`,
		status, nilIfEmpty(errMsg), model.NowUTC(), jobID)
	q.mu.Unlock()
}

// RequeueOrphaned runs at boot: this process is the only writer, so a job still
// marked running belongs to a dead process.
func (q *Queue) RequeueOrphaned() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(`UPDATE jobs SET status=?, lease_expires_at=NULL, updated_at=? WHERE status=?`,
		StatusQueued, model.NowUTC(), StatusRunning)
	if err != nil {
		q.log.Error("requeue orphaned jobs", "err", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// ReapExpired requeues jobs whose worker died without reporting.
func (q *Queue) ReapExpired() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := model.NowUTC()
	res, err := q.db.Exec(`UPDATE jobs SET status=?, lease_expires_at=NULL, updated_at=?
		WHERE status=? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		StatusQueued, now, StatusRunning, now)
	if err != nil {
		q.log.Error("reap expired leases", "err", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// Counts feeds /health so queue depth is observable without a metrics stack.
func (q *Queue) Counts() map[string]int {
	out := map[string]int{StatusQueued: 0, StatusRunning: 0, StatusDone: 0, StatusFailed: 0}
	rows, err := q.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// RetireExperiment fails an experiment that no worker will ever finish.
func (q *Queue) RetireExperiment(expID, reason string) {
	q.mu.Lock()
	_, _ = q.db.Exec(`UPDATE experiments SET status='failed', summary=? WHERE id=? AND status='running'`,
		summaryJSON(reason), expID)
	q.mu.Unlock()
}

func (q *Queue) heartbeat() time.Duration {
	if q.lease < 3 {
		return time.Second
	}
	return q.lease / 3
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func summaryJSON(reason string) string {
	b, _ := json.Marshal(map[string]string{"error": reason})
	return string(b)
}

func trimForLog(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}
