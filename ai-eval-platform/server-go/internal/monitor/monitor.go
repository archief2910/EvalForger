// Package monitor evaluates aggregate conditions over a window of production
// traffic.
//
// A per-trace online rule answers "is this one response good?". A monitor
// answers a different question: "is the fleet healthy right now?" — an error
// rate over the last fifteen minutes, a p95 latency, an average score. The
// condition is evaluated against a metric computed over a time window, and it
// fires once when the condition becomes true rather than once per evaluation.
//
// The two ideas that make this a product rather than a query:
//
//   - State. A monitor that stays in breach produces one event, not one per
//     tick. It resolves when the metric recovers. An operator wants to be told
//     once, and told again when it is fixed.
//   - Notification. An event that nobody is told about is a log line. A
//     monitor can deliver to a webhook or a Slack channel, and the delivery
//     result is recorded on the event so a failed notification is visible
//     instead of silent.
//
// Planes: monitors and their events are control rows. The metrics they watch
// are telemetry, computed by a query in the telemetry plane and merged here.
package monitor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"evalforge/internal/model"
)

// Metric names a quantity a monitor can watch. The set is deliberately small:
// every metric here is computable with one query against the telemetry plane
// and is meaningful without a join.
const (
	MetricErrorRate   = "error_rate"
	MetricAvgScore    = "avg_score"
	MetricAvgLatency  = "avg_latency_ms"
	MetricP95Latency  = "p95_latency_ms"
	MetricAvgCost     = "avg_cost_usd"
	MetricSpanCount   = "span_count"
	MetricTraceCount  = "trace_count"
	MetricErrorCount  = "error_count"
	metricDefaultUnit = ""
)

// cutoffTime renders a millisecond epoch as a timestamp string the telemetry
// plane can compare. The telemetry tables store timestamps as text, so the
// comparison has to happen on the same scale the values were written on.
func cutoffTime(cutoffMS int64) string {
	return time.UnixMilli(cutoffMS).UTC().Format(time.RFC3339Nano)
}

// Metrics lists every supported metric, for validation and for the UI.
var Metrics = []struct {
	Name        string
	Description string
}{
	{MetricErrorRate, "Share of traces in the window that ended in an error"},
	{MetricAvgScore, "Average automatic score over the window"},
	{MetricAvgLatency, "Average root-span latency in the window"},
	{MetricP95Latency, "95th percentile root-span latency in the window"},
	{MetricAvgCost, "Average cost per trace in the window"},
	{MetricSpanCount, "Number of spans in the window"},
	{MetricTraceCount, "Number of traces in the window"},
	{MetricErrorCount, "Number of errored traces in the window"},
}

// MetricNames returns the supported metric names.
func MetricNames() []string {
	out := make([]string, 0, len(Metrics))
	for _, m := range Metrics {
		out = append(out, m.Name)
	}
	return out
}

// Operators compare a metric against a threshold.
const (
	OpGreater = ">"
	OpLess    = "<"
)

// CanonicalOperator maps the spellings a user might type onto one symbol.
// Opik learned this the hard way: an operator stored as `less_than` but read
// back as `>` silently reverses what an alert fires on.
func CanonicalOperator(op string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case ">", "greater_than", "gte", ">=":
		return OpGreater, true
	case "<", "less_than", "lte", "<=":
		return OpLess, true
	default:
		return "", false
	}
}

// Compare applies the operator to a value and a threshold.
func Compare(op string, value, threshold float64) bool {
	switch op {
	case OpGreater:
		return value > threshold
	case OpLess:
		return value < threshold
	default:
		return false
	}
}

// Monitor is one aggregate condition over a window.
type Monitor struct {
	ID            string
	ProjectID     string
	Name          string
	Enabled       bool
	Metric        string
	Operator      string
	Threshold     float64
	WindowSeconds int
	// ChannelType is "none", "webhook" or "slack".
	ChannelType   string
	ChannelURL    string
	ChannelSecret string
	// State is "ok" or "firing". It is the memory that makes a monitor fire
	// once per episode instead of once per tick.
	State           string
	LastValue       *float64
	LastFiredAt     string
	LastEvaluatedAt string
	CreatedAt       string
	UpdatedAt       string
}

// Event is one firing episode of a monitor.
type Event struct {
	ID                string
	MonitorID         string
	ProjectID         string
	Metric            string
	Operator          string
	Threshold         float64
	Value             *float64
	State             string
	FiredAt           string
	ResolvedAt        string
	Notified          bool
	NotificationError string
}

// Notifier delivers a monitor event to a channel. The interface exists so the
// evaluation logic can be tested without a network, and so a deployment can
// add another channel without touching the state machine.
type Notifier interface {
	// Notify delivers the event. It returns an error when the delivery did
	// not succeed; the caller records it on the event.
	Notify(ctx context.Context, m Monitor, e Event) error
}

// Store owns monitors, their events, and the evaluation loop.
type Store struct {
	control   *sql.DB
	telemetry *sql.DB
	log       *slog.Logger

	notifierMu sync.RWMutex
	notifier   Notifier
	httpClient *http.Client

	// evalMu serialises evaluations: two concurrent passes could both see the
	// monitor as "ok" and both fire it.
	evalMu sync.Mutex
}

// New builds a store. A nil notifier means events are recorded but not
// delivered, which is the honest default for a monitor without a channel.
func New(control, telemetry *sql.DB, log *slog.Logger) *Store {
	return &Store{
		control:    control,
		telemetry:  telemetry,
		log:        log,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetNotifier swaps the delivery backend (tests, or a deployment with a
// different channel).
func (s *Store) SetNotifier(n Notifier) {
	s.notifierMu.Lock()
	s.notifier = n
	s.notifierMu.Unlock()
}

func (s *Store) currentNotifier() Notifier {
	s.notifierMu.RLock()
	defer s.notifierMu.RUnlock()
	return s.notifier
}

// ---------- persistence ----------

const monitorColumns = `id, project_id, name, enabled, metric, operator, threshold, window_seconds,
	channel_type, channel_url, channel_secret, state, last_value, last_fired_at, last_evaluated_at,
	created_at, updated_at`

func scanMonitor(sc interface{ Scan(...interface{}) error }) (Monitor, error) {
	var m Monitor
	var enabled int
	var lastValue sql.NullFloat64
	var lastFired, lastEvaluated sql.NullString
	err := sc.Scan(&m.ID, &m.ProjectID, &m.Name, &enabled, &m.Metric, &m.Operator, &m.Threshold,
		&m.WindowSeconds, &m.ChannelType, &m.ChannelURL, &m.ChannelSecret, &m.State,
		&lastValue, &lastFired, &lastEvaluated, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return Monitor{}, err
	}
	m.Enabled = enabled != 0
	if lastValue.Valid {
		v := lastValue.Float64
		m.LastValue = &v
	}
	m.LastFiredAt = lastFired.String
	m.LastEvaluatedAt = lastEvaluated.String
	return m, nil
}

// List returns every monitor for a project.
func (s *Store) List(ctx context.Context, projectID string) ([]Monitor, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT `+monitorColumns+` FROM monitors WHERE project_id=? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one monitor scoped to its project.
func (s *Store) Get(ctx context.Context, projectID, id string) (Monitor, error) {
	row := s.control.QueryRowContext(ctx,
		`SELECT `+monitorColumns+` FROM monitors WHERE id=? AND project_id=?`, id, projectID)
	return scanMonitor(row)
}

// Insert stores a new monitor and fills its id, timestamps and default state.
func (s *Store) Insert(ctx context.Context, m *Monitor) error {
	now := model.NowUTC()
	m.ID = model.NewID()
	m.State = "ok"
	m.CreatedAt, m.UpdatedAt = now, now
	_, err := s.control.ExecContext(ctx,
		`INSERT INTO monitors(id, project_id, name, enabled, metric, operator, threshold, window_seconds,
		 channel_type, channel_url, channel_secret, state, last_value, last_fired_at, last_evaluated_at,
		 created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,'ok',NULL,NULL,NULL,?,?)`,
		m.ID, m.ProjectID, m.Name, boolInt(m.Enabled), m.Metric, m.Operator, m.Threshold,
		m.WindowSeconds, m.ChannelType, m.ChannelURL, m.ChannelSecret, m.CreatedAt, m.UpdatedAt)
	return err
}

// Save updates the editable fields of a monitor. The state machine is not
// editable: only the evaluation loop moves it.
func (s *Store) Save(ctx context.Context, m Monitor) error {
	m.UpdatedAt = model.NowUTC()
	_, err := s.control.ExecContext(ctx,
		`UPDATE monitors SET name=?, enabled=?, metric=?, operator=?, threshold=?, window_seconds=?,
		 channel_type=?, channel_url=?, channel_secret=?, updated_at=?
		 WHERE id=? AND project_id=?`,
		m.Name, boolInt(m.Enabled), m.Metric, m.Operator, m.Threshold, m.WindowSeconds,
		m.ChannelType, m.ChannelURL, m.ChannelSecret, m.UpdatedAt, m.ID, m.ProjectID)
	return err
}

// Delete removes a monitor. Its events stay as the audit trail.
func (s *Store) Delete(ctx context.Context, projectID, id string) error {
	_, err := s.control.ExecContext(ctx,
		`DELETE FROM monitors WHERE id=? AND project_id=?`, id, projectID)
	return err
}

// Events lists a project's monitor events, newest first.
func (s *Store) Events(ctx context.Context, projectID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.control.QueryContext(ctx,
		`SELECT id, monitor_id, project_id, metric, operator, threshold, value, state, fired_at,
		 COALESCE(resolved_at,''), notified, COALESCE(notification_error,'')
		 FROM monitor_events WHERE project_id=? ORDER BY fired_at DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var value sql.NullFloat64
		var notified int
		if err := rows.Scan(&e.ID, &e.MonitorID, &e.ProjectID, &e.Metric, &e.Operator, &e.Threshold,
			&value, &e.State, &e.FiredAt, &e.ResolvedAt, &notified, &e.NotificationError); err != nil {
			return nil, err
		}
		if value.Valid {
			v := value.Float64
			e.Value = &v
		}
		e.Notified = notified != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- metrics ----------

// MetricValue computes a monitor's metric over its window, ending at now.
// The window is inclusive of the last `window_seconds` of telemetry.
func (s *Store) MetricValue(ctx context.Context, m Monitor, now time.Time) (float64, error) {
	cutoff := now.Add(-time.Duration(m.WindowSeconds) * time.Second).UnixMilli()
	switch m.Metric {
	case MetricErrorRate:
		return s.errorRate(ctx, m.ProjectID, cutoff)
	case MetricAvgScore:
		return s.avgScore(ctx, m.ProjectID, cutoff)
	case MetricAvgLatency:
		return s.avgLatency(ctx, m.ProjectID, cutoff)
	case MetricP95Latency:
		return s.p95Latency(ctx, m.ProjectID, cutoff)
	case MetricAvgCost:
		return s.avgCost(ctx, m.ProjectID, cutoff)
	case MetricSpanCount:
		return s.count(ctx, `SELECT COUNT(*) FROM spans WHERE project_id=? AND start_ms>=?`, m.ProjectID, cutoff)
	case MetricTraceCount:
		return s.count(ctx, `SELECT COUNT(DISTINCT trace_id) FROM spans WHERE project_id=? AND start_ms>=?`, m.ProjectID, cutoff)
	case MetricErrorCount:
		return s.count(ctx, `SELECT COUNT(DISTINCT trace_id) FROM spans
			WHERE project_id=? AND start_ms>=? AND status='error'`, m.ProjectID, cutoff)
	default:
		return 0, fmt.Errorf("unknown metric %q", m.Metric)
	}
}

// errorRate is the share of distinct traces in the window that contain an
// errored root span. Counting traces rather than spans keeps one noisy trace
// from dominating the ratio.
func (s *Store) errorRate(ctx context.Context, projectID string, cutoff int64) (float64, error) {
	var total, errored int
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT trace_id) FROM spans WHERE project_id=? AND start_ms>=?`,
		projectID, cutoff).Scan(&total); err != nil {
		return 0, err
	}
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT trace_id) FROM spans
		 WHERE project_id=? AND start_ms>=? AND status='error'`, projectID, cutoff).Scan(&errored); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}
	return float64(errored) / float64(total), nil
}

func (s *Store) avgScore(ctx context.Context, projectID string, cutoff int64) (float64, error) {
	var avg sql.NullFloat64
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT AVG(score) FROM online_scores
		 WHERE project_id=? AND CAST(created_at AS TIMESTAMP) >= CAST(? AS TIMESTAMP)
		   AND score IS NOT NULL`,
		projectID, cutoffTime(cutoff)).Scan(&avg); err != nil {
		return 0, err
	}
	if !avg.Valid {
		return 0, nil
	}
	return avg.Float64, nil
}

func (s *Store) avgLatency(ctx context.Context, projectID string, cutoff int64) (float64, error) {
	var avg sql.NullFloat64
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT AVG(latency_ms) FROM spans
		 WHERE project_id=? AND start_ms>=? AND parent_id IS NULL`,
		projectID, cutoff).Scan(&avg); err != nil {
		return 0, err
	}
	if !avg.Valid {
		return 0, nil
	}
	return avg.Float64, nil
}

// p95Latency uses the nearest-rank percentile computed in SQL, so the whole
// window is scanned once instead of being pulled into Go and sorted.
func (s *Store) p95Latency(ctx context.Context, projectID string, cutoff int64) (float64, error) {
	var p95 sql.NullFloat64
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT quantile_cont(latency_ms, 0.95) FROM spans
		 WHERE project_id=? AND start_ms>=? AND parent_id IS NULL`,
		projectID, cutoff).Scan(&p95); err != nil {
		return 0, err
	}
	if !p95.Valid {
		return 0, nil
	}
	return p95.Float64, nil
}

func (s *Store) avgCost(ctx context.Context, projectID string, cutoff int64) (float64, error) {
	var avg sql.NullFloat64
	if err := s.telemetry.QueryRowContext(ctx,
		`SELECT SUM(cost_usd)/NULLIF(COUNT(DISTINCT trace_id),0) FROM spans
		 WHERE project_id=? AND start_ms>=?`, projectID, cutoff).Scan(&avg); err != nil {
		return 0, err
	}
	if !avg.Valid {
		return 0, nil
	}
	return avg.Float64, nil
}

func (s *Store) count(ctx context.Context, query string, args ...interface{}) (float64, error) {
	var n int
	if err := s.telemetry.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return float64(n), nil
}

// ---------- evaluation ----------

// Evaluation is the outcome of checking one monitor.
type Evaluation struct {
	Monitor Monitor `json:"monitor"`
	Value   float64 `json:"value"`
	Firing  bool    `json:"firing"`
	Changed bool    `json:"changed"`
	Event   *Event  `json:"event,omitempty"`
	Reason  string  `json:"reason,omitempty"`
}

// EvaluateAll checks every enabled monitor and returns what happened. The
// state transition is what makes a monitor fire once: ok → firing raises an
// event, firing → ok resolves it, and no transition means no new event.
func (s *Store) EvaluateAll(ctx context.Context, now time.Time) ([]Evaluation, error) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()

	monitors, err := s.enabledMonitors(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Evaluation, 0, len(monitors))
	for _, m := range monitors {
		ev, err := s.evaluate(ctx, m, now)
		if err != nil {
			s.log.Warn("monitor evaluation failed", "monitor_id", m.ID, "err", err)
			ev = Evaluation{Monitor: m, Reason: err.Error()}
		}
		out = append(out, ev)
	}
	return out, nil
}

// Evaluate checks one monitor, by id.
func (s *Store) Evaluate(ctx context.Context, projectID, id string, now time.Time) (Evaluation, error) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()

	m, err := s.Get(ctx, projectID, id)
	if err != nil {
		return Evaluation{}, err
	}
	return s.evaluate(ctx, m, now)
}

// enabledMonitors lists every enabled monitor across all projects.
func (s *Store) enabledMonitors(ctx context.Context) ([]Monitor, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT `+monitorColumns+` FROM monitors WHERE enabled=1 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// evaluate computes the metric and applies the state transition.
func (s *Store) evaluate(ctx context.Context, m Monitor, now time.Time) (Evaluation, error) {
	value, err := s.MetricValue(ctx, m, now)
	if err != nil {
		return Evaluation{Monitor: m, Reason: err.Error()}, err
	}
	firing := Compare(m.Operator, value, m.Threshold)
	ev := Evaluation{Monitor: m, Value: value, Firing: firing}

	nowStr := model.NowUTC()
	switch {
	case firing && m.State != "firing":
		// The condition just became true: this is the edge, and it is the only
		// place an event is created.
		event, err := s.openEvent(ctx, m, value, nowStr)
		if err != nil {
			return ev, err
		}
		ev.Changed = true
		ev.Event = &event
		if err := s.setState(ctx, m.ID, "firing", &value, nowStr, true); err != nil {
			return ev, err
		}
	case !firing && m.State == "firing":
		// The condition recovered: resolve the open event.
		if err := s.resolveEvent(ctx, m, value, nowStr); err != nil {
			return ev, err
		}
		ev.Changed = true
		if err := s.setState(ctx, m.ID, "ok", &value, nowStr, false); err != nil {
			return ev, err
		}
	default:
		// No transition: record the value so the UI can show the trend, and
		// leave the state alone.
		if err := s.setState(ctx, m.ID, m.State, &value, nowStr, false); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// setState writes the monitor's state and last observed value.
func (s *Store) setState(ctx context.Context, id, state string, value *float64, now string, fired bool) error {
	var firedAt interface{}
	if fired {
		firedAt = now
	} else {
		firedAt = nil
	}
	_, err := s.control.ExecContext(ctx,
		`UPDATE monitors SET state=?, last_value=?, last_evaluated_at=?,
		 last_fired_at=COALESCE(?, last_fired_at)
		 WHERE id=?`, state, value, now, firedAt, id)
	return err
}

// openEvent creates the event for a new firing episode.
func (s *Store) openEvent(ctx context.Context, m Monitor, value float64, now string) (Event, error) {
	e := Event{
		ID: model.NewID(), MonitorID: m.ID, ProjectID: m.ProjectID, Metric: m.Metric,
		Operator: m.Operator, Threshold: m.Threshold, Value: &value, State: "firing", FiredAt: now,
	}
	_, err := s.control.ExecContext(ctx,
		`INSERT INTO monitor_events(id, monitor_id, project_id, metric, operator, threshold, value,
		 state, fired_at, notified, notification_error)
		 VALUES (?,?,?,?,?,?,?, 'firing', ?, 0, '')`,
		e.ID, e.MonitorID, e.ProjectID, e.Metric, e.Operator, e.Threshold, value, now)
	if err != nil {
		return e, err
	}
	s.deliver(ctx, m, e)
	return e, nil
}

// resolveEvent closes the open event for a monitor.
func (s *Store) resolveEvent(ctx context.Context, m Monitor, value float64, now string) error {
	_, err := s.control.ExecContext(ctx,
		`UPDATE monitor_events SET state='resolved', resolved_at=?, value=?
		 WHERE monitor_id=? AND state='firing'`, now, value, m.ID)
	return err
}

// deliver sends the event to the monitor's channel and records the outcome.
func (s *Store) deliver(ctx context.Context, m Monitor, e Event) {
	notifier := s.currentNotifier()
	if notifier == nil || m.ChannelType == "" || m.ChannelType == "none" {
		return
	}
	err := notifier.Notify(ctx, m, e)
	notified := 0
	msg := ""
	if err != nil {
		msg = err.Error()
		s.log.Warn("monitor notification failed", "monitor_id", m.ID, "channel", m.ChannelType, "err", err)
	} else {
		notified = 1
		s.log.Info("monitor notification sent", "monitor_id", m.ID, "channel", m.ChannelType)
	}
	_, _ = s.control.ExecContext(ctx,
		`UPDATE monitor_events SET notified=?, notification_error=? WHERE id=?`, notified, msg, e.ID)
	e.Notified = notified != 0
	e.NotificationError = msg
}

// ---------- notification ----------

// WebhookNotifier delivers a monitor event to an HTTP endpoint.
//
// The payload is the same for every channel type, so a receiver can parse one
// shape. Slack is delivered as a Slack message rather than a raw JSON post,
// because a raw post to a Slack webhook renders as a blank message.
type WebhookNotifier struct {
	Client *http.Client
}

// Notify posts the event to the monitor's channel URL.
func (n *WebhookNotifier) Notify(ctx context.Context, m Monitor, e Event) error {
	if m.ChannelURL == "" {
		return fmt.Errorf("monitor has no channel url")
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	payload := map[string]interface{}{
		"event_id":    e.ID,
		"monitor_id":  m.ID,
		"monitor":     m.Name,
		"project_id":  m.ProjectID,
		"state":       e.State,
		"metric":      e.Metric,
		"operator":    e.Operator,
		"threshold":   e.Threshold,
		"value":       e.Value,
		"window_secs": m.WindowSeconds,
		"fired_at":    e.FiredAt,
		"message":     fmt.Sprintf("%s: %s %v %v (window %ds)", m.Name, e.Metric, e.Operator, e.Threshold, m.WindowSeconds),
	}
	if m.ChannelType == "slack" {
		text, _ := payload["message"].(string)
		if e.Value != nil {
			text = fmt.Sprintf("%s — %s = %s (threshold %s %s)", m.Name, e.Metric,
				formatNumber(*e.Value), e.Operator, formatNumber(e.Threshold))
		}
		payload = map[string]interface{}{
			"text": text,
			"blocks": []map[string]interface{}{
				{
					"type": "section",
					"text": map[string]string{"type": "mrkdwn", "text": text},
				},
			},
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.ChannelURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EvalForge-Monitor/1")
	// A shared secret lets the receiver verify the payload came from us.
	if m.ChannelSecret != "" {
		mac := hmac.New(sha256.New, []byte(m.ChannelSecret))
		mac.Write(body)
		req.Header.Set("X-EvalForge-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver notification: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification endpoint returned %s", resp.Status)
	}
	return nil
}

func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

// Stats is a small summary for the dashboard.
type Stats struct {
	Monitors       int      `json:"monitors"`
	Enabled        int      `json:"enabled"`
	Firing         int      `json:"firing"`
	OpenEvents     int      `json:"open_events"`
	Unnotified     int      `json:"unnotified"`
	DeliveryErrors int      `json:"delivery_errors"`
	AvgValue       *float64 `json:"avg_value"`
}

// ProjectStats summarises a project's monitors.
func (s *Store) ProjectStats(ctx context.Context, projectID string) Stats {
	stats := Stats{}
	monitors, err := s.List(ctx, projectID)
	if err != nil {
		return stats
	}
	stats.Monitors = len(monitors)
	for _, m := range monitors {
		if m.Enabled {
			stats.Enabled++
		}
		if m.State == "firing" {
			stats.Firing++
		}
	}
	var open, unnotified, deliveryErrors int
	_ = s.control.QueryRowContext(ctx,
		`SELECT COUNT(*) FILTER (WHERE state='firing'),
		        COUNT(*) FILTER (WHERE notified=0 AND state='firing'),
		        COUNT(*) FILTER (WHERE notification_error<>'')
		 FROM monitor_events WHERE project_id=?`, projectID).
		Scan(&open, &unnotified, &deliveryErrors)
	stats.OpenEvents = open
	stats.Unnotified = unnotified
	stats.DeliveryErrors = deliveryErrors
	return stats
}
