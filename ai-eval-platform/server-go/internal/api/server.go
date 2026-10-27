// Package api contains the HTTP surface: routing, middleware, handlers and the
// auth/OTLP/MCP protocol implementations. Handlers are methods on Server, so
// dependencies are explicit and a test can construct the same object the
// binary does.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"evalforge/internal/config"
	"evalforge/internal/guardrails"
	"evalforge/internal/model"
	"evalforge/internal/monitor"
	"evalforge/internal/online"
	"evalforge/internal/queue"
	"evalforge/internal/retention"
	"evalforge/internal/sandbox"
	"evalforge/internal/worker"
)

// Server is the application object. Wiring lives in cmd/server; this type owns
// the two database handles, the durable queue and the process-boundary clients.
//
// DB is the control plane (metadata, jobs, credentials). Telemetry is the
// append-heavy plane (spans, feedback, eval runs, conversations). Each plane
// has its own write mutex so a slow telemetry write never blocks metadata.
type Server struct {
	DB          *sql.DB
	Telemetry   *sql.DB
	dbMu        sync.Mutex // serialises control-plane write transactions
	telemetryMu sync.Mutex // serialises telemetry-plane write transactions

	cfg config.Config
	log *slog.Logger

	queue  *queue.Queue
	worker *worker.Client
	// online scores live traffic against user-defined rules. It runs off the
	// ingest path, bounded, and owns no request state.
	online *online.Engine
	// retention owns per-project telemetry policies and the sweeper that
	// enforces them.
	retention *retention.Store
	// monitors evaluate aggregate conditions over a window of traffic and
	// notify a channel when one starts firing.
	monitors *monitor.Store
	// sandbox executes user-authored metrics; it is the only path from stored
	// code to execution.
	sandbox sandbox.Runner
	// guardrails enforce runtime safety policies on ingest and on request.
	guardrails *guardrails.Store

	// baseURL is this process's own origin, used by OAuth discovery and the
	// worker callback. Tests point it at httptest.Server.
	baseURL string
	// mcpBaseURL is where the MCP tool layer sends API calls. In HTTP mode it
	// loops back to this server; in stdio mode it can be a remote deployment.
	mcpBaseURL string
	mcpAPIKey  string
	mcpHTTP    *http.Client

	// internalToken authenticates the evaluation worker's own loopback calls
	// when AUTH_MODE=required. It is generated at boot, passed only to the
	// child process and never logged or persisted.
	internalToken string

	// now is the clock. Tests replace it so a monitor window can be evaluated
	// against a fixed point in time.
	now func() time.Time
}

// New constructs the API surface. Start() is called separately so tests can
// exercise handlers without launching the dispatcher or scrubber.
func New(control, telemetry *sql.DB, cfg config.Config, log *slog.Logger) *Server {
	s := &Server{
		DB:            control,
		Telemetry:     telemetry,
		cfg:           cfg,
		log:           log,
		baseURL:       cfg.Issuer(),
		mcpBaseURL:    cfg.MCPAPIBase,
		mcpAPIKey:     cfg.MCPAPIKey,
		mcpHTTP:       &http.Client{Timeout: 60 * time.Second},
		internalToken: randToken(32),
		now:           time.Now,
	}
	s.worker = &worker.Client{
		ScriptPath:    cfg.WorkerPath,
		Python:        cfg.PythonBin,
		BaseURL:       cfg.ServerBaseURL,
		Timeout:       cfg.WorkerTimeout,
		InternalToken: s.internalToken,
		Log:           log,
	}
	s.online = online.New(control, telemetry, &worker.Scorer{Client: s.worker, Timeout: cfg.OnlineTimeout},
		log, cfg.OnlineConcurrency, cfg.OnlineTimeout)
	s.retention = retention.New(control, telemetry, log)
	s.monitors = monitor.New(control, telemetry, log)
	s.monitors.SetNotifier(&monitor.WebhookNotifier{})
	s.guardrails = guardrails.New(control, telemetry)
	s.sandbox = selectSandbox(cfg, log)
	s.online.SetCustomScorer(&customMetricBackend{s: s, run: s.sandbox, time: cfg.OnlineTimeout})
	sandboxDescriber = s.sandbox.Describe
	return s
}

// selectSandbox picks the isolation backend. Docker is the only one that
// counts as isolation; the local subprocess is opt-in and refuses untrusted
// code unless the operator has explicitly accepted the risk.
func selectSandbox(cfg config.Config, log *slog.Logger) sandbox.Runner {
	if cfg.SandboxUnsafeLocal {
		log.Warn("custom metrics run in a local subprocess: this is NOT an isolation boundary and must not be used in production")
		return &sandbox.LocalRunner{AllowUnsafe: true, Python: cfg.PythonBin, Timeout: cfg.OnlineTimeout}
	}
	docker := sandbox.NewDockerRunner()
	if cfg.SandboxImage != "" {
		docker.Image = cfg.SandboxImage
	}
	if !docker.Available() {
		// Not an error: a deployment that does not use custom metrics does not
		// need Docker. The API reports the sandbox as unavailable, and any
		// attempt to run a custom metric fails closed.
		log.Info("custom metric sandbox unavailable (docker not reachable); custom metrics will not run")
	}
	return docker
}

// NewControlOnly builds a Server with just the control plane.
//
// It is for maintenance commands (minting a key) that must run alongside a live
// server: DuckDB permits one writer process per file, so opening telemetry here
// would fail for a command that never reads it.
func NewControlOnly(control *sql.DB, cfg config.Config, log *slog.Logger) *Server {
	return &Server{
		DB:      control,
		cfg:     cfg,
		log:     log,
		baseURL: cfg.Issuer(),
		now:     time.Now,
	}
}

// SetOnlineScorer swaps the scoring backend. Tests use it to keep matching and
// persistence deterministic without spawning Python.
func (s *Server) SetOnlineScorer(sc online.Scorer) { s.online.SetScorer(sc) }

// StopOnline cancels queued and in-flight online evaluations and waits.
func (s *Server) StopOnline() { s.online.Stop() }

// CreateAPIKey mints a credential outside the HTTP surface. It backs the
// `--create-key` bootstrap path; the raw key is returned once and never stored.
func (s *Server) CreateAPIKey(name, owner, projectID string, scopes map[string]bool, ttl time.Duration) (raw, id, prefix string, err error) {
	raw, key, err := s.insertAPIKey(name, owner, scopes, projectID, ttl)
	if err != nil {
		return "", "", "", err
	}
	return raw, key.ID, key.Prefix, nil
}

// AttachQueue connects the dispatcher after construction (the queue needs a
// handler from this server, so one of the two references must be set later).
func (s *Server) AttachQueue(q *queue.Queue) { s.queue = q }

// Queue returns the attached queue (tests use it for health assertions).
func (s *Server) Queue() *queue.Queue { return s.queue }

// WriteMutex exposes the control-plane SQLite writer lock to the queue.
func (s *Server) WriteMutex() *sync.Mutex { return &s.dbMu }

// TelemetryMutex exposes the telemetry-plane writer lock to background work.
func (s *Server) TelemetryMutex() *sync.Mutex { return &s.telemetryMu }

// SetBaseURL is used by tests that start an httptest server on a random port.
// Production wiring gets the same value from config.
func (s *Server) SetBaseURL(u string) {
	s.baseURL = u
	s.mcpBaseURL = u
	s.cfg.ServerBaseURL = u
	s.cfg.PublicBaseURL = u
	if s.worker != nil {
		s.worker.BaseURL = u
	}
}

// Handler returns the complete middleware chain around the router.
func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.authenticate(s.Router()))
}

// Start begins background workers: job dispatch and credential scrubbing.
func (s *Server) Start(ctx context.Context) *sync.WaitGroup {
	if s.queue != nil {
		if n := s.queue.RequeueOrphaned(); n > 0 {
			s.logf("requeued orphaned jobs", "count", n)
		}
	}
	return s.startBackground(ctx)
}

func (s *Server) startBackground(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	if s.queue != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qw := s.queue.Start(ctx)
			qw.Wait()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.scrubAuth()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.scrubAuth()
			}
		}
	}()
	// Conversation lifecycle: close threads that have been idle longer than
	// THREAD_IDLE_SECONDS. New activity reopens them automatically.
	if s.Telemetry != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sweepThreads(ctx)
		}()
	}
	// Retention: expire telemetry older than each project's policy. The
	// interval is configurable because a demo wants to see it happen quickly
	// and a deployment does not want a sweep every minute.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.sweepRetention(ctx)
	}()
	// Monitors: evaluate aggregate conditions over a window of traffic.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.evaluateMonitors(ctx)
	}()
	return &wg
}

// evaluateMonitors checks every monitor on a schedule.
func (s *Server) evaluateMonitors(ctx context.Context) {
	interval := s.cfg.MonitorInterval
	if interval <= 0 {
		interval = time.Minute
	}
	// Evaluate once at boot so a monitor that is already in breach is
	// reported without waiting a full interval.
	s.runMonitorEvaluation(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runMonitorEvaluation(ctx)
		}
	}
}

// runMonitorEvaluation evaluates all monitors and logs the state changes.
func (s *Server) runMonitorEvaluation(ctx context.Context) {
	if s.monitors == nil {
		return
	}
	results, err := s.monitors.EvaluateAll(ctx, s.now())
	if err != nil {
		s.log.Warn("monitor evaluation failed", "err", err)
		return
	}
	for _, ev := range results {
		if ev.Changed {
			s.logf("monitor state changed",
				"monitor_id", ev.Monitor.ID, "metric", ev.Monitor.Metric,
				"value", ev.Value, "operator", ev.Monitor.Operator,
				"threshold", ev.Monitor.Threshold, "firing", ev.Firing)
		}
	}
}

// sweepRetention enforces retention policies on a schedule.
func (s *Server) sweepRetention(ctx context.Context) {
	interval := s.cfg.RetentionSweep
	if interval <= 0 {
		interval = time.Hour
	}
	// Catch up on data that aged past its policy while the process was down.
	s.applyRetention(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.applyRetention(ctx)
		}
	}
}

// applyRetention runs one sweep and logs what it removed.
func (s *Server) applyRetention(ctx context.Context) {
	if s.retention == nil {
		return
	}
	results, err := s.retention.Sweep(ctx)
	if err != nil {
		s.log.Warn("retention sweep failed", "err", err)
		return
	}
	for _, r := range results {
		s.logf("retention applied",
			"project_id", r.ProjectID, "retention_days", r.RetentionDays,
			"spans", r.SpansDeleted, "feedback", r.FeedbackGone,
			"scores", r.ScoresGone, "threads", r.ThreadsDeleted)
	}
}

// RunJob is the queue handler: decode the job payload and run one worker.
func (s *Server) RunJob(ctx context.Context, j queue.Job) queue.Outcome {
	if j.Kind == queue.KindThreadEval {
		return s.runThreadJob(ctx, j)
	}
	if j.Kind == queue.KindOptimize {
		return s.runOptimizeJob(ctx, j)
	}
	var in model.EvalInput
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
		return queue.Outcome{Diagnostic: "bad job payload: " + err.Error()}
	}
	out := s.worker.Run(ctx, worker.Request{ExperimentID: j.ExperimentID, ProjectID: j.ProjectID, Input: in})
	if out.Diagnostic == "" && !out.TimedOut {
		// The worker reports results over HTTP before exiting. If it exited
		// cleanly without a callback, fail the experiment with a reason rather
		// than leaving it "running" forever.
		var status string
		_ = s.DB.QueryRow(`SELECT status FROM experiments WHERE id=?`, j.ExperimentID).Scan(&status)
		if status == "running" {
			s.retireExperiment(j.ExperimentID, "worker exited without reporting results")
		}
	}
	return queue.Outcome(out)
}

// runThreadJob executes one thread evaluation. The worker reads the
// conversation over HTTP, computes thread scorers and POSTs them back to the
// thread callback. A clean exit without any score is a failed attempt, so the
// queue can retry instead of silently doing nothing.
func (s *Server) runThreadJob(ctx context.Context, j queue.Job) queue.Outcome {
	var in model.ThreadEvalInput
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
		return queue.Outcome{Diagnostic: "bad thread job payload: " + err.Error()}
	}
	threadID := in.ThreadID
	if threadID == "" {
		threadID = j.ThreadID
	}
	out := s.worker.RunThread(ctx, worker.ThreadRequest{
		JobID: j.ID, ProjectID: j.ProjectID, ThreadID: threadID, ScorerNames: in.ScorerNames,
	})
	if out.Diagnostic == "" && !out.TimedOut && s.Telemetry != nil {
		var n int
		_ = s.Telemetry.QueryRow(`SELECT COUNT(*) FROM thread_scores WHERE job_id=?`, j.ID).Scan(&n)
		if n == 0 {
			return queue.Outcome{Diagnostic: "worker exited without reporting thread scores"}
		}
	}
	return queue.Outcome(out)
}

// retireExperiment is the queue's failure callback.
func (s *Server) RetireExperiment(expID, reason string) { s.retireExperiment(expID, reason) }

func (s *Server) retireExperiment(expID, reason string) {
	s.dbMu.Lock()
	_, _ = s.DB.Exec(`UPDATE experiments SET status='failed', summary=? WHERE id=? AND status='running'`,
		j(map[string]string{"error": reason}), expID)
	s.dbMu.Unlock()
}

// ---------- small shared helpers ----------

// idSeq makes generated ids unique even when the wall clock has coarse
// granularity (Windows time.Now() resolution is ~15ms).
var idSeq atomic.Uint64

func newID() string {
	return time.Now().UTC().Format("20060102150405.000000000") + "-" + strconv.FormatUint(idSeq.Add(1), 10)
}

func j(v interface{}) string { return model.MarshalJSONValue(v) }

func parseJSONPtr(s string) interface{} {
	if s == "" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// nilIfEmpty returns nil for an empty string, so an absent value and an empty
// one serialise the same way.
func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// readAll reads a bounded request body.
func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func jsonOrNull(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	return j(v)
}

func (s *Server) logf(msg string, args ...any) { s.log.Info(msg, args...) }
