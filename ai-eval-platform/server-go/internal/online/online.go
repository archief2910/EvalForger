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
	// Scope is "trace" or "thread". A trace rule scores one request; a thread
	// rule scores a whole conversation when it closes. The two need different
	// scorers and different subjects for their alerts, so the scope is part of
	// the rule rather than something inferred from the scorer names.
	Scope     string
	CreatedAt string
	UpdatedAt string
}

// Trace scope and thread scope.
const (
	ScopeTrace  = "trace"
	ScopeThread = "thread"
)

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

// ThreadScoreRequest asks a scorer backend to evaluate one conversation.
type ThreadScoreRequest struct {
	ProjectID string
	ThreadID  string
	Scorers   []string
}

// Scorer executes the scorers for a trace or a conversation. The worker
// subprocess implements this; tests substitute a fake so matching and
// persistence stay deterministic.
type Scorer interface {
	ScoreTrace(ctx context.Context, req ScoreRequest) ([]Score, error)
	ScoreThread(ctx context.Context, req ThreadScoreRequest) ([]Score, error)
}

// CustomScorer executes user-authored metrics. It is a separate interface from
// Scorer on purpose: the trusted worker must never be handed code a user
// wrote, and a custom metric must never run anywhere except the sandbox.
//
// The engine decides which names belong to which backend, so the boundary is
// visible at exactly one point in the code.
type CustomScorer interface {
	// Names returns the custom metric names a project has registered.
	Names(ctx context.Context, projectID string) (map[string]bool, error)
	// ScoreTrace runs one custom metric against a stored trace.
	ScoreTrace(ctx context.Context, projectID, traceID, name string) (Score, error)
	// ScoreThread runs one custom metric against a stored conversation.
	ScoreThread(ctx context.Context, projectID, threadID, name string) (Score, error)
}

// Engine owns rule persistence and the bounded execution of online scoring.
type Engine struct {
	control   *sql.DB
	telemetry *sql.DB
	log       *slog.Logger

	scorerMu sync.RWMutex
	scorer   Scorer
	// custom runs user-authored metrics. It is nil until a deployment wires a
	// sandbox; when nil, a rule that names a custom metric fails closed rather
	// than silently scoring nothing.
	custom CustomScorer

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

// SetCustomScorer wires the sandbox-backed backend for user metrics. Passing
// nil is meaningful: custom metrics then fail instead of running.
func (e *Engine) SetCustomScorer(c CustomScorer) {
	e.scorerMu.Lock()
	e.custom = c
	e.scorerMu.Unlock()
}

func (e *Engine) currentScorer() Scorer {
	e.scorerMu.RLock()
	defer e.scorerMu.RUnlock()
	return e.scorer
}

func (e *Engine) currentCustom() CustomScorer {
	e.scorerMu.RLock()
	defer e.scorerMu.RUnlock()
	return e.custom
}

// Stop cancels queued and in-flight evaluations and waits for them to exit.
// It runs before the database handles are closed at shutdown.
func (e *Engine) Stop() {
	e.cancel()
	e.wg.Wait()
}

// ---------- rule persistence ----------

const ruleColumns = `id, project_id, name, enabled, sample_rate, filter_status, filter_model,
	filter_name, scorers, alert_threshold, scope, created_at, updated_at`

func scanRule(sc interface{ Scan(...interface{}) error }) (Rule, error) {
	var r Rule
	var enabled int
	var scorers string
	var threshold sql.NullFloat64
	err := sc.Scan(&r.ID, &r.ProjectID, &r.Name, &enabled, &r.SampleRate, &r.FilterStatus,
		&r.FilterModel, &r.FilterName, &scorers, &threshold, &r.Scope, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Rule{}, err
	}
	r.Enabled = enabled != 0
	if r.Scope == "" {
		r.Scope = ScopeTrace
	}
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
	if r.Scope == "" {
		r.Scope = ScopeTrace
	}
	scorers, _ := json.Marshal(r.Scorers)
	_, err := e.control.ExecContext(ctx,
		`INSERT INTO online_rules(id, project_id, name, enabled, sample_rate, filter_status,
		 filter_model, filter_name, scorers, alert_threshold, scope, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ProjectID, r.Name, boolInt(r.Enabled), r.SampleRate, r.FilterStatus,
		r.FilterModel, r.FilterName, string(scorers), r.AlertThreshold, r.Scope, r.CreatedAt, r.UpdatedAt)
	return err
}

// Save updates the mutable fields of an existing rule.
func (e *Engine) Save(ctx context.Context, r Rule) error {
	scorers, _ := json.Marshal(r.Scorers)
	r.UpdatedAt = model.NowUTC()
	if r.Scope == "" {
		r.Scope = ScopeTrace
	}
	_, err := e.control.ExecContext(ctx,
		`UPDATE online_rules SET name=?, enabled=?, sample_rate=?, filter_status=?, filter_model=?,
		 filter_name=?, scorers=?, alert_threshold=?, scope=?, updated_at=?
		 WHERE id=? AND project_id=?`,
		r.Name, boolInt(r.Enabled), r.SampleRate, r.FilterStatus, r.FilterModel,
		r.FilterName, string(scorers), r.AlertThreshold, r.Scope, r.UpdatedAt, r.ID, r.ProjectID)
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
// trace-scoped rule and schedules scoring without blocking the ingest
// response.
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
		if !r.Enabled || len(r.Scorers) == 0 || r.Scope != ScopeTrace {
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

// AfterThreadClose is called when a conversation closes. A thread-scoped rule
// scores the conversation as a whole at that point, because "how did this
// conversation go" is only answerable once the conversation is over.
//
// The trigger is the close, not each turn: scoring a two-turn prefix of a
// twenty-turn conversation would produce a score that changes meaning as the
// conversation grows.
func (e *Engine) AfterThreadClose(projectID, threadID string) {
	if threadID == "" {
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
		if !r.Enabled || len(r.Scorers) == 0 || r.Scope != ScopeThread {
			continue
		}
		if !r.Sampled(threadID) {
			continue
		}
		if e.threadAlreadyScored(ctx, r.ID, threadID) {
			continue
		}
		e.dispatchThread(r, threadID)
	}
}

func (e *Engine) alreadyScored(ctx context.Context, ruleID, traceID string) bool {
	var n int
	err := e.telemetry.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM online_scores WHERE rule_id=? AND trace_id=?`, ruleID, traceID).Scan(&n)
	return err == nil && n > 0
}

func (e *Engine) threadAlreadyScored(ctx context.Context, ruleID, threadID string) bool {
	var n int
	err := e.telemetry.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM online_thread_scores WHERE rule_id=? AND thread_id=?`, ruleID, threadID).Scan(&n)
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

// dispatchThread schedules a conversation scoring run.
func (e *Engine) dispatchThread(rule Rule, threadID string) {
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
		if _, err := e.runThread(ctx, rule, threadID); err != nil {
			e.log.Warn("online thread scoring failed", "rule_id", rule.ID, "thread_id", threadID, "err", err)
		}
	}()
}

// RunThread executes a thread-scoped rule synchronously.
func (e *Engine) RunThread(ctx context.Context, rule Rule, threadID string) ([]Score, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	return e.runThread(ctx, rule, threadID)
}

// runThread scores one conversation and persists the result.
func (e *Engine) runThread(ctx context.Context, rule Rule, threadID string) ([]Score, error) {
	builtin, custom, err := e.splitScorers(ctx, rule)
	if err != nil {
		return nil, err
	}
	scores := []Score{}
	if len(builtin) > 0 {
		scorer := e.currentScorer()
		if scorer == nil {
			return nil, fmt.Errorf("no scorer backend configured")
		}
		s, err := scorer.ScoreThread(ctx, ThreadScoreRequest{
			ProjectID: rule.ProjectID, ThreadID: threadID, Scorers: builtin,
		})
		if err != nil {
			return nil, err
		}
		scores = append(scores, s...)
	}
	for _, name := range custom {
		s, err := e.runCustomThread(ctx, rule, threadID, name)
		if err != nil {
			return scores, err
		}
		scores = append(scores, s)
	}
	if err := e.persistThread(ctx, rule, threadID, scores); err != nil {
		return scores, err
	}
	return scores, nil
}

// persistThread writes one row per scorer and raises an alert when a score
// falls below the rule's threshold. The alert points at the conversation, not
// a trace, so a reader can open the conversation that went wrong.
func (e *Engine) persistThread(ctx context.Context, rule Rule, threadID string, scores []Score) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	for _, s := range scores {
		now := model.NowUTC()
		res, err := e.telemetry.ExecContext(ctx,
			`INSERT INTO online_thread_scores(id, project_id, rule_id, thread_id, scorer, score, rationale, judge_mode, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			newID(), rule.ProjectID, rule.ID, threadID, s.Scorer, s.Score, s.Rationale, s.JudgeMode, now)
		if err != nil {
			return fmt.Errorf("store thread score %s: %w", s.Scorer, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if rule.AlertThreshold != nil && s.Score != nil && *s.Score < *rule.AlertThreshold {
			if _, err := e.control.ExecContext(ctx,
				`INSERT INTO online_alerts(id, project_id, rule_id, trace_id, subject_type, subject_id, scorer, score, threshold, status, created_at)
				 VALUES (?,?,?,?,?,?,?,?,?,'open',?) ON CONFLICT DO NOTHING`,
				newID(), rule.ProjectID, rule.ID, "", "thread", threadID, s.Scorer, *s.Score, *rule.AlertThreshold, now); err != nil {
				return fmt.Errorf("raise thread alert: %w", err)
			}
		}
	}
	return nil
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
	builtin, custom, err := e.splitScorers(ctx, rule)
	if err != nil {
		return nil, err
	}
	scores := []Score{}
	if len(builtin) > 0 {
		scorer := e.currentScorer()
		if scorer == nil {
			return nil, fmt.Errorf("no scorer backend configured")
		}
		s, err := scorer.ScoreTrace(ctx, ScoreRequest{
			ProjectID: rule.ProjectID, TraceID: traceID, Scorers: builtin,
		})
		if err != nil {
			return nil, err
		}
		scores = append(scores, s...)
	}
	for _, name := range custom {
		s, err := e.runCustomTrace(ctx, rule, traceID, name)
		if err != nil {
			return scores, err
		}
		scores = append(scores, s)
	}
	if err := e.persist(ctx, rule, traceID, scores); err != nil {
		return scores, err
	}
	return scores, nil
}

// splitScorers partitions a rule's scorers into the trusted registry and the
// project's custom metrics. The split is by name, resolved once per run, so a
// metric added or removed between runs takes effect without a restart.
func (e *Engine) splitScorers(ctx context.Context, rule Rule) (builtin, custom []string, err error) {
	registry := e.currentCustom()
	if registry == nil {
		return rule.Scorers, nil, nil
	}
	names, err := registry.Names(ctx, rule.ProjectID)
	if err != nil {
		// A metrics table that cannot be read must not silently turn a custom
		// metric into an unknown builtin; the run fails instead.
		return nil, nil, fmt.Errorf("resolve custom metrics: %w", err)
	}
	for _, name := range rule.Scorers {
		if names[name] {
			custom = append(custom, name)
		} else {
			builtin = append(builtin, name)
		}
	}
	return builtin, custom, nil
}

// runCustomTrace executes one user metric and normalises its verdict.
func (e *Engine) runCustomTrace(ctx context.Context, rule Rule, traceID, name string) (Score, error) {
	registry := e.currentCustom()
	if registry == nil {
		return Score{}, fmt.Errorf("custom metric %q cannot run: no sandbox is configured", name)
	}
	s, err := registry.ScoreTrace(ctx, rule.ProjectID, traceID, name)
	if err != nil {
		return Score{Scorer: name, Rationale: err.Error(), JudgeMode: "sandbox_error"}, err
	}
	if s.Scorer == "" {
		s.Scorer = name
	}
	return s, nil
}

// runCustomThread is the conversation analogue of runCustomTrace.
func (e *Engine) runCustomThread(ctx context.Context, rule Rule, threadID, name string) (Score, error) {
	registry := e.currentCustom()
	if registry == nil {
		return Score{}, fmt.Errorf("custom metric %q cannot run: no sandbox is configured", name)
	}
	s, err := registry.ScoreThread(ctx, rule.ProjectID, threadID, name)
	if err != nil {
		return Score{Scorer: name, Rationale: err.Error(), JudgeMode: "sandbox_error"}, err
	}
	if s.Scorer == "" {
		s.Scorer = name
	}
	return s, nil
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
				`INSERT INTO online_alerts(id, project_id, rule_id, trace_id, subject_type, subject_id, scorer, score, threshold, status, created_at)
				 VALUES (?,?,?,?,?,?,?,?,?,'open',?) ON CONFLICT DO NOTHING`,
				newID(), rule.ProjectID, rule.ID, traceID, "trace", traceID, s.Scorer, *s.Score, *rule.AlertThreshold, now); err != nil {
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
