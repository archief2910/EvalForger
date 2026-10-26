package api

// Online evaluation: rules that score live traffic, and the alerts their
// thresholds raise. Rule state and alerts are control-plane rows; the scores
// they produce are telemetry rows. Nothing joins across the two engines — the
// API reads both and merges, exactly like the trace view does.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"evalforge/internal/model"
	"evalforge/internal/online"
)

type ruleIn struct {
	Name           string   `json:"name"`
	Enabled        *bool    `json:"enabled"`
	SampleRate     *float64 `json:"sample_rate"`
	FilterStatus   string   `json:"filter_status"`
	FilterModel    string   `json:"filter_model"`
	FilterName     string   `json:"filter_name"`
	Scorers        []string `json:"scorers"`
	AlertThreshold *float64 `json:"alert_threshold"`
}

// recordScorers is the subset of the registry that can run on a single trace.
// Thread scorers are excluded: online evaluation scores one trace at a time.
func recordScorers() map[string]bool {
	out := map[string]bool{}
	for _, sc := range builtinScorers {
		if sc["scope"] == "record" {
			out[sc["name"].(string)] = true
		}
	}
	return out
}

func validateRule(in ruleIn) string {
	if strings.TrimSpace(in.Name) == "" {
		return "name is required"
	}
	if len(in.Name) > 120 {
		return "name is too long (max 120)"
	}
	if len(in.Scorers) == 0 {
		return "at least one scorer is required"
	}
	known := recordScorers()
	seen := map[string]bool{}
	for _, name := range in.Scorers {
		if !known[name] {
			return "unknown or non-trace scorer: " + name
		}
		if seen[name] {
			return "duplicate scorer: " + name
		}
		seen[name] = true
	}
	if in.SampleRate != nil && (*in.SampleRate < 0 || *in.SampleRate > 1) {
		return "sample_rate must be between 0 and 1"
	}
	if in.AlertThreshold != nil && (*in.AlertThreshold < 0 || *in.AlertThreshold > 1) {
		return "alert_threshold must be between 0 and 1 (0 disables alerts)"
	}
	return ""
}

func ruleJSON(r online.Rule) map[string]interface{} {
	threshold := interface{}(nil)
	if r.AlertThreshold != nil {
		threshold = *r.AlertThreshold
	}
	return map[string]interface{}{
		"id": r.ID, "project_id": r.ProjectID, "name": r.Name, "enabled": r.Enabled,
		"sample_rate": r.SampleRate,
		"filter": map[string]string{
			"status": r.FilterStatus, "model": r.FilterModel, "name": r.FilterName,
		},
		"scorers": r.Scorers, "alert_threshold": threshold,
		"created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
	}
}

func (s *Server) handleOnlineRules(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		rules, err := s.online.List(r.Context(), projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(rules))
		for _, rule := range rules {
			out = append(out, ruleJSON(rule))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in ruleIn
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateRule(in); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		sample := 1.0
		if in.SampleRate != nil {
			sample = *in.SampleRate
		}
		rule := online.Rule{
			ProjectID: projectID, Name: in.Name, Enabled: enabled, SampleRate: sample,
			FilterStatus: in.FilterStatus, FilterModel: in.FilterModel, FilterName: in.FilterName,
			Scorers: in.Scorers, AlertThreshold: thresholdOrNil(in.AlertThreshold),
		}
		if err := s.online.Insert(r.Context(), &rule); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, ruleJSON(rule))
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func thresholdOrNil(v *float64) *float64 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

func (s *Server) handleOnlineRule(w http.ResponseWriter, r *http.Request, projectID, ruleID string) {
	rule, err := s.online.Get(r.Context(), projectID, ruleID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "rule not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, ruleJSON(rule))
	case http.MethodPatch, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			errJSON(w, 400, "read body: "+err.Error())
			return
		}
		var in ruleIn
		if err := json.Unmarshal(body, &in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		var sent map[string]json.RawMessage
		_ = json.Unmarshal(body, &sent)
		// Merge: only provided fields change, so a UI toggle does not have to
		// re-send the whole rule. Presence is checked on the raw object because
		// an explicitly empty filter and an absent one mean different things.
		if _, ok := sent["name"]; ok {
			rule.Name = in.Name
		}
		if _, ok := sent["enabled"]; ok && in.Enabled != nil {
			rule.Enabled = *in.Enabled
		}
		if _, ok := sent["sample_rate"]; ok && in.SampleRate != nil {
			rule.SampleRate = *in.SampleRate
		}
		_, hasStatus := sent["filter_status"]
		_, hasModel := sent["filter_model"]
		_, hasName := sent["filter_name"]
		if hasStatus || hasModel || hasName {
			rule.FilterStatus, rule.FilterModel, rule.FilterName =
				in.FilterStatus, in.FilterModel, in.FilterName
		}
		if _, ok := sent["scorers"]; ok {
			rule.Scorers = in.Scorers
		}
		if _, ok := sent["alert_threshold"]; ok {
			rule.AlertThreshold = thresholdOrNil(in.AlertThreshold)
		}
		check := ruleIn{
			Name: rule.Name, Scorers: rule.Scorers,
			SampleRate: &rule.SampleRate, AlertThreshold: rule.AlertThreshold,
		}
		if msg := validateRule(check); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		if err := s.online.Save(r.Context(), rule); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, ruleJSON(rule))
	case http.MethodDelete:
		if err := s.online.Delete(r.Context(), projectID, ruleID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": ruleID})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func (s *Server) handleOnlineRuleRun(w http.ResponseWriter, r *http.Request, projectID, ruleID string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	var in struct {
		TraceID string `json:"trace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, "bad json: "+err.Error())
		return
	}
	if in.TraceID == "" {
		errJSON(w, 400, "trace_id is required")
		return
	}
	rule, err := s.online.Get(r.Context(), projectID, ruleID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "rule not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var n int
	if err := s.Telemetry.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM spans WHERE project_id=? AND trace_id=?`, projectID, in.TraceID).Scan(&n); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if n == 0 {
		errJSON(w, 404, "trace not found")
		return
	}
	scores, err := s.online.Run(r.Context(), rule, in.TraceID, true)
	if err != nil {
		errJSON(w, 502, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(scores))
	for _, sc := range scores {
		item := map[string]interface{}{
			"scorer": sc.Scorer, "score": sc.Score,
			"rationale": sc.Rationale, "judge_mode": sc.JudgeMode,
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]interface{}{"trace_id": in.TraceID, "scores": out})
}

type alertRow struct {
	ID, ProjectID, RuleID, RuleName, TraceID, Scorer, Status, CreatedAt string
	Score, Threshold                                                    float64
	AcknowledgedAt                                                      sql.NullString
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		errJSON(w, 405, "method not allowed")
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}
	query := `SELECT a.id, a.project_id, a.rule_id, COALESCE(r.name,''), a.trace_id, a.scorer,
		a.score, a.threshold, a.status, a.created_at, a.acknowledged_at
		FROM online_alerts a LEFT JOIN online_rules r ON r.id=a.rule_id
		WHERE a.project_id=?`
	args := []interface{}{projectID}
	if status != "all" {
		query += ` AND a.status=?`
		args = append(args, status)
	}
	query += ` ORDER BY a.created_at DESC LIMIT 100`
	rows, err := s.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var a alertRow
		var score sql.NullFloat64
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.RuleID, &a.RuleName, &a.TraceID, &a.Scorer,
			&score, &a.Threshold, &a.Status, &a.CreatedAt, &a.AcknowledgedAt); err != nil {
			continue
		}
		item := map[string]interface{}{
			"id": a.ID, "project_id": a.ProjectID, "rule_id": a.RuleID, "rule_name": a.RuleName,
			"trace_id": a.TraceID, "scorer": a.Scorer, "score": score.Float64,
			"threshold": a.Threshold, "status": a.Status, "created_at": a.CreatedAt,
		}
		if score.Valid {
			item["score"] = score.Float64
		} else {
			item["score"] = nil
		}
		if a.AcknowledgedAt.Valid {
			item["acknowledged_at"] = a.AcknowledgedAt.String
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]interface{}{"alerts": out, "status": status})
}

func (s *Server) handleAlertAck(w http.ResponseWriter, r *http.Request, projectID, alertID string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	now := model.NowUTC()
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE online_alerts SET status='acknowledged', acknowledged_at=?
		 WHERE id=? AND project_id=? AND status='open'`, now, alertID, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either it does not exist or it was already acknowledged; answer 404
		// only for the former so a double-click stays idempotent.
		var status string
		if err := s.DB.QueryRowContext(r.Context(),
			`SELECT status FROM online_alerts WHERE id=? AND project_id=?`, alertID, projectID).
			Scan(&status); err != nil {
			errJSON(w, 404, "alert not found")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"id": alertID, "status": status})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"id": alertID, "status": "acknowledged", "acknowledged_at": now})
}

// onlineScoresForTrace returns automatic scores for the trace view, keyed with
// the rule name so a reader does not have to resolve ids.
func (s *Server) onlineScoresForTrace(ctx context.Context, projectID, traceID string) []map[string]interface{} {
	out := []map[string]interface{}{}
	rows, err := s.Telemetry.QueryContext(ctx,
		`SELECT rule_id, scorer, score, rationale, judge_mode, created_at
		 FROM online_scores WHERE project_id=? AND trace_id=? ORDER BY created_at`, projectID, traceID)
	if err != nil {
		return out
	}
	defer rows.Close()
	names := map[string]string{}
	if rrows, err := s.DB.QueryContext(ctx,
		`SELECT id, name FROM online_rules WHERE project_id=?`, projectID); err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var id, name string
			if rrows.Scan(&id, &name) == nil {
				names[id] = name
			}
		}
	}
	for rows.Next() {
		var ruleID, scorer, rationale, judgeMode, createdAt string
		var score sql.NullFloat64
		if rows.Scan(&ruleID, &scorer, &score, &rationale, &judgeMode, &createdAt) != nil {
			continue
		}
		item := map[string]interface{}{
			"rule_id": ruleID, "rule_name": names[ruleID], "scorer": scorer,
			"rationale": rationale, "judge_mode": judgeMode, "created_at": createdAt,
		}
		if score.Valid {
			item["score"] = score.Float64
		} else {
			item["score"] = nil
		}
		out = append(out, item)
	}
	return out
}

// onlineStats feeds the dashboard block. Control rows (rules, alerts) and
// telemetry rows (scores) are counted separately and merged here.
func (s *Server) onlineStats(ctx context.Context, projectID string, since time.Time) map[string]interface{} {
	stats := map[string]interface{}{
		"rules_total": 0, "rules_enabled": 0, "alerts_open": 0,
		"scored": 0, "avg_score": nil,
	}
	var total, enabled int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM online_rules WHERE project_id=?`, projectID).
		Scan(&total, &enabled); err == nil {
		stats["rules_total"] = total
		stats["rules_enabled"] = enabled
	}
	var open int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM online_alerts WHERE project_id=? AND status='open'`, projectID).
		Scan(&open); err == nil {
		stats["alerts_open"] = open
	}
	var scored int
	var avg sql.NullFloat64
	if err := s.Telemetry.QueryRowContext(ctx,
		`SELECT COUNT(*), AVG(score) FROM online_scores
		 WHERE project_id=? AND created_at>=? AND score IS NOT NULL`,
		projectID, since.UTC().Format(time.RFC3339Nano)).Scan(&scored, &avg); err == nil {
		stats["scored"] = scored
		if avg.Valid {
			stats["avg_score"] = avg.Float64
		}
	}
	return stats
}
