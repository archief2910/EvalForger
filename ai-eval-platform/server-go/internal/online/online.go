// Package online evaluates live traffic against user-defined rules.
//
// It is the second half of the evaluation loop. Dataset experiments answer
// "did this change help?" for a batch someone chose; online rules answer
// "is production healthy right now?" for every trace that arrives, without a
// human promoting it to a dataset first.
//
// The engine is deliberately thin on execution: scoring lives in the Python
// worker (the scorer registry is there), this package owns matching, sampling,
// concurrency and the two writes that make a result durable — a score in the
// telemetry plane and, when the score crosses the rule's threshold, an alert in
// the control plane. The planes are never joined in SQL; the API merges them.
package online

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"evalforge/internal/model"
)

// Rule is one online-evaluation rule. Filters are simple equality/substring
// tests on the trace root; sampling keeps a rule affordable on high volume.
type Rule struct {
	ID             string
	ProjectID      string
	Name           string
	Enabled        bool
	SampleRate     float64
	FilterStatus   string
	FilterModel    string
	FilterName     string
	Scorers        []string
	AlertThreshold *float64
	CreatedAt      string
	UpdatedAt      string
}

// TraceInfo is the small projection of a trace a rule can match on. It is
// computed from the batch that was just ingested, so matching costs no query.
type TraceInfo struct {
	ID     string
	Name   string
	Status string
	Models []string
}

// Matches reports whether the rule applies to the trace.
func (r Rule) Matches(t TraceInfo) bool {
	if r.FilterStatus != "" && !strings.EqualFold(r.FilterStatus, t.Status) {
		return false
	}
	if r.FilterName != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(r.FilterName)) {
		return false
	}
	if r.FilterModel != "" {
		found := false
		for _, m := range t.Models {
			if strings.EqualFold(m, r.FilterModel) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Sampled decides, deterministically, whether this trace is evaluated. Hashing
// rule+trace means a retried ingest makes the same decision, and a low sample
// rate does not turn into "score the same trace twice".
func (r Rule) Sampled(traceID string) bool {
	if r.SampleRate >= 1 {
		return true
	}
	if r.SampleRate <= 0 {
		return false
	}
	sum := sha256.Sum256([]byte(r.ID + "|" + traceID))
	v := float64(binary.BigEndian.Uint64(sum[:8])) / float64(^uint64(0))
	return v < r.SampleRate
}

// Score is one scorer's verdict on a trace.
type Score struct {
	Scorer    string   `json:"name"`
	Score     *float64 `json:"score"`
	Rationale string   `json:"rationale"`
	JudgeMode string   `json:"judge_mode"`
}

// ScoreRequest asks a scorer backend to evaluate one trace.
type ScoreRequest struct {
	ProjectID string
	TraceID   string
	Scorers   []string
}

// Scorer executes the scorers for a trace. The worker subprocess implements
// this; tests substitute a fake so matching and persistence stay deterministic.
type Scorer interface {
	ScoreTrace(ctx context.Context, req ScoreRequest) ([]Score, error)
}

// Engine owns rule persistence and the bounded execution of online scoring.
type Engine struct {
	control   *sql.DB
	telemetry *sql.DB
	log       *slog.Logger

	scorerMu sync.RWMutex
	scorer   Scorer

	sem     chan struct{}
	timeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// writeMu serialises telemetry/control writes from scoring goroutines. The
	// two writes are not one transaction across planes (by design), but they
	// are not interleaved with each other either.
	writeMu sync.Mutex
}

// New builds an engine. concurrency bounds how many traces are scored at once;
// a slow LLM judge must not starve ingest or stampede the provider.
func New(control, telemetry *sql.DB, scorer Scorer, log *slog.Logger, concurrency int, timeout time.Duration) *Engine {
	if concurrency < 1 {
		concurrency = 1
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		control: control, telemetry: telemetry, log: log,
		scorer: scorer, sem: make(chan struct{}, concurrency), timeout: timeout,
		ctx: ctx, cancel: cancel,
	}
}

// SetScorer replaces the execution backend (tests, or a future sandboxed one).
func (e *Engine) SetScorer(s Scorer) {
	e.scorerMu.Lock()
	e.scorer = s
	e.scorerMu.Unlock()
}

func (e *Engine) currentScorer() Scorer {
	e.scorerMu.RLock()
	defer e.scorerMu.RUnlock()
	return e.scorer
}

// Stop cancels queued and in-flight evaluations and waits for them to exit.
// It runs before the database handles are closed at shutdown.
func (e *Engine) Stop() {
	e.cancel()
	e.wg.Wait()
}

// ---------- rule persistence ----------

const ruleColumns = `id, project_id, name, enabled, sample_rate, filter_status, filter_model,
	filter_name, scorers, alert_threshold, created_at, updated_at`

func scanRule(sc interface{ Scan(...interface{}) error }) (Rule, error) {
	var r Rule
	var enabled int
	var scorers string
	var threshold sql.NullFloat64
	err := sc.Scan(&r.ID, &r.ProjectID, &r.Name, &enabled, &r.SampleRate, &r.FilterStatus,
		&r.FilterModel, &r.FilterName, &scorers, &threshold, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Rule{}, err
	}
	r.Enabled = enabled != 0
	if threshold.Valid {
		v := threshold.Float64
		r.AlertThreshold = &v
	}
	if err := json.Unmarshal([]byte(scorers), &r.Scorers); err != nil {
		r.Scorers = []string{}
	}
	return r, nil
}

// List returns every rule for a project, disabled ones included.
func (e *Engine) List(ctx context.Context, projectID string) ([]Rule, error) {
	rows, err := e.control.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM online_rules WHERE project_id=? ORDER BY created_at DESC`, projectID)
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
func (e *Engine) Get(ctx context.Context, projectID, id string) (Rule, error) {
	row := e.control.QueryRowContext(ctx,
		`SELECT `+ruleColumns+` FROM online_rules WHERE id=? AND project_id=?`, id, projectID)
	return scanRule(row)
}

// Insert stores a new rule and fills ID/timestamps.
func (e *Engine) Insert(ctx context.Context, r *Rule) error {
	now := model.NowUTC()
	r.ID = newID()
	r.CreatedAt, r.UpdatedAt = now, now
	scorers, _ := json.Marshal(r.Scorers)
	_, err := e.control.ExecContext(ctx,
		`INSERT INTO online_rules(id, project_id, name, enabled, sample_rate, filter_status,
		 filter_model, filter_name, scorers, alert_threshold, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ProjectID, r.Name, boolInt(r.Enabled), r.SampleRate, r.FilterStatus,
		r.FilterModel, r.FilterName, string(scorers), r.AlertThreshold, r.CreatedAt, r.UpdatedAt)
	return err
}

// Save updates the mutable fields of an existing rule.
func (e *Engine) Save(ctx context.Context, r Rule) error {
	scorers, _ := json.Marshal(r.Scorers)
	r.UpdatedAt = model.NowUTC()
	_, err := e.control.ExecContext(ctx,
		`UPDATE online_rules SET name=?, enabled=?, sample_rate=?, filter_status=?, filter_model=?,
		 filter_name=?, scorers=?, alert_threshold=?, updated_at=? WHERE id=? AND project_id=?`,
		r.Name, boolInt(r.Enabled), r.SampleRate, r.FilterStatus, r.FilterModel,
		r.FilterName, string(scorers), r.AlertThreshold, r.UpdatedAt, r.ID, r.ProjectID)
	return err
}

// Delete removes a rule. Alerts it already raised stay for the audit trail.
func (e *Engine) Delete(ctx context.Context, projectID, id string) error {
	_, err := e.control.ExecContext(ctx,
		`DELETE FROM online_rules WHERE id=? AND project_id=?`, id, projectID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- execution ----------

// AfterIngest is called once a batch is committed. It matches every enabled
// rule and schedules scoring without blocking the ingest response.
func (e *Engine) AfterIngest(projectID string, traces []TraceInfo) {
	if len(traces) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	rules, err := e.List(ctx, projectID)
	if err != nil {
		e.log.Warn("online rules lookup failed", "project_id", projectID, "err", err)
		return
	}
	for _, r := range rules {
		if !r.Enabled || len(r.Scorers) == 0 {
			continue
		}
		for _, t := range traces {
			if !r.Matches(t) || !r.Sampled(t.ID) {
				continue
			}
			if e.alreadyScored(ctx, r.ID, t.ID) {
				continue
			}
			e.dispatch(r, t.ID, false)
		}
	}
}

func (e *Engine) alreadyScored(ctx context.Context, ruleID, traceID string) bool {
	var n int
	err := e.telemetry.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM online_scores WHERE rule_id=? AND trace_id=?`, ruleID, traceID).Scan(&n)
	return err == nil && n > 0
}

func (e *Engine) dispatch(rule Rule, traceID string, force bool) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case e.sem <- struct{}{}:
			defer func() { <-e.sem }()
		case <-e.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(e.ctx, e.timeout)
		defer cancel()
		if _, err := e.run(ctx, rule, traceID, force); err != nil {
			e.log.Warn("online scoring failed", "rule_id", rule.ID, "trace_id", traceID, "err", err)
		}
	}()
}

// Run executes a rule synchronously. force skips the "already scored" guard and
// is what the manual "run on this trace" endpoint uses.
func (e *Engine) Run(ctx context.Context, rule Rule, traceID string, force bool) ([]Score, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	return e.run(ctx, rule, traceID, force)
}

func (e *Engine) run(ctx context.Context, rule Rule, traceID string, force bool) ([]Score, error) {
	if !force && e.alreadyScored(ctx, rule.ID, traceID) {
		return nil, nil
	}
	scorer := e.currentScorer()
	if scorer == nil {
		return nil, fmt.Errorf("no scorer backend configured")
	}
	scores, err := scorer.ScoreTrace(ctx, ScoreRequest{
		ProjectID: rule.ProjectID, TraceID: traceID, Scorers: rule.Scorers,
	})
	if err != nil {
		return nil, err
	}
	if err := e.persist(ctx, rule, traceID, scores); err != nil {
		return scores, err
	}
	return scores, nil
}

// persist writes one row per scorer and raises an alert when a score falls
// below the rule's threshold. ON CONFLICT makes retries and manual reruns safe;
// an alert is only raised for a score row that was actually inserted.
func (e *Engine) persist(ctx context.Context, rule Rule, traceID string, scores []Score) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	for _, s := range scores {
		now := model.NowUTC()
		res, err := e.telemetry.ExecContext(ctx,
			`INSERT INTO online_scores(id, project_id, rule_id, trace_id, scorer, score, rationale, judge_mode, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			newID(), rule.ProjectID, rule.ID, traceID, s.Scorer, s.Score, s.Rationale, s.JudgeMode, now)
		if err != nil {
			return fmt.Errorf("store score %s: %w", s.Scorer, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if rule.AlertThreshold != nil && s.Score != nil && *s.Score < *rule.AlertThreshold {
			if _, err := e.control.ExecContext(ctx,
				`INSERT INTO online_alerts(id, project_id, rule_id, trace_id, scorer, score, threshold, status, created_at)
				 VALUES (?,?,?,?,?,?,?,'open',?) ON CONFLICT DO NOTHING`,
				newID(), rule.ProjectID, rule.ID, traceID, s.Scorer, *s.Score, *rule.AlertThreshold, now); err != nil {
				return fmt.Errorf("raise alert: %w", err)
			}
		}
	}
	return nil
}

// Wait blocks until every scheduled evaluation has finished. Tests use it;
// shutdown calls Stop, which cancels first.
func (e *Engine) Wait() { e.wg.Wait() }

var idSeq atomic.Uint64

func newID() string {
	return time.Now().UTC().Format("20060102150405.000000000") + "-" + itoa(idSeq.Add(1))
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
