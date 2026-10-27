package api

// Windowed monitors: aggregate conditions over production traffic.
//
// A monitor watches a metric computed over a time window ("error rate above
// 5% in the last 15 minutes"). It fires once when the condition becomes true
// and resolves when it recovers, which is what makes it a monitor rather than
// another per-trace score.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"evalforge/internal/monitor"
)

type monitorIn struct {
	Name          string   `json:"name"`
	Enabled       *bool    `json:"enabled"`
	Metric        string   `json:"metric"`
	Operator      string   `json:"operator"`
	Threshold     *float64 `json:"threshold"`
	WindowSeconds *int     `json:"window_seconds"`
	ChannelType   string   `json:"channel_type"`
	ChannelURL    string   `json:"channel_url"`
	ChannelSecret string   `json:"channel_secret"`
}

func validateMonitor(in monitorIn) string {
	if strings.TrimSpace(in.Name) == "" {
		return "name is required"
	}
	if len(in.Name) > 120 {
		return "name is too long (max 120)"
	}
	if !isKnownMetric(in.Metric) {
		return "unknown metric: " + in.Metric + " (supported: " + strings.Join(monitor.MetricNames(), ", ") + ")"
	}
	op, ok := monitor.CanonicalOperator(in.Operator)
	if !ok {
		return "operator must be > or <"
	}
	_ = op
	if in.Threshold == nil {
		return "threshold is required"
	}
	if in.WindowSeconds != nil && (*in.WindowSeconds < 60 || *in.WindowSeconds > 86400*7) {
		return "window_seconds must be between 60 and 604800"
	}
	switch in.ChannelType {
	case "", "none", "webhook", "slack":
	default:
		return "channel_type must be none, webhook or slack"
	}
	if (in.ChannelType == "webhook" || in.ChannelType == "slack") && strings.TrimSpace(in.ChannelURL) == "" {
		return "channel_url is required for a webhook or slack channel"
	}
	if in.ChannelURL != "" && !strings.HasPrefix(in.ChannelURL, "https://") && !strings.HasPrefix(in.ChannelURL, "http://") {
		return "channel_url must be an http(s) URL"
	}
	return ""
}

func isKnownMetric(name string) bool {
	for _, m := range monitor.MetricNames() {
		if m == name {
			return true
		}
	}
	return false
}

func monitorJSON(m monitor.Monitor) map[string]interface{} {
	out := map[string]interface{}{
		"id": m.ID, "project_id": m.ProjectID, "name": m.Name, "enabled": m.Enabled,
		"metric": m.Metric, "operator": m.Operator, "threshold": m.Threshold,
		"window_seconds": m.WindowSeconds, "channel_type": m.ChannelType,
		"state": m.State, "created_at": m.CreatedAt, "updated_at": m.UpdatedAt,
	}
	// The channel secret is write-only: it is stored so the payload can be
	// signed, but it is never handed back to a reader.
	out["has_channel_secret"] = m.ChannelSecret != ""
	if m.LastValue != nil {
		out["last_value"] = *m.LastValue
	}
	if m.LastFiredAt != "" {
		out["last_fired_at"] = m.LastFiredAt
	}
	if m.LastEvaluatedAt != "" {
		out["last_evaluated_at"] = m.LastEvaluatedAt
	}
	return out
}

func monitorEventJSON(e monitor.Event) map[string]interface{} {
	out := map[string]interface{}{
		"id": e.ID, "monitor_id": e.MonitorID, "project_id": e.ProjectID,
		"metric": e.Metric, "operator": e.Operator, "threshold": e.Threshold,
		"state": e.State, "fired_at": e.FiredAt, "notified": e.Notified,
	}
	if e.Value != nil {
		out["value"] = *e.Value
	}
	if e.ResolvedAt != "" {
		out["resolved_at"] = e.ResolvedAt
	}
	if e.NotificationError != "" {
		out["notification_error"] = e.NotificationError
	}
	return out
}

func (s *Server) handleMonitors(w http.ResponseWriter, r *http.Request, projectID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		monitors, err := s.monitors.List(r.Context(), projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(monitors))
		for _, m := range monitors {
			out = append(out, monitorJSON(m))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in monitorIn
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateMonitor(in); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		op, _ := monitor.CanonicalOperator(in.Operator)
		m := monitor.Monitor{
			ProjectID: projectID, Name: in.Name, Enabled: true,
			Metric: in.Metric, Operator: op, Threshold: *in.Threshold,
			WindowSeconds: 900, ChannelType: in.ChannelType, ChannelURL: in.ChannelURL,
			ChannelSecret: in.ChannelSecret,
		}
		if in.Enabled != nil {
			m.Enabled = *in.Enabled
		}
		if in.WindowSeconds != nil {
			m.WindowSeconds = *in.WindowSeconds
		}
		if m.ChannelType == "" {
			m.ChannelType = "none"
		}
		if err := s.monitors.Insert(r.Context(), &m); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, monitorJSON(m))
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request, projectID, monitorID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	m, err := s.monitors.Get(r.Context(), projectID, monitorID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "monitor not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, monitorJSON(m))
	case http.MethodPatch, http.MethodPost:
		var in monitorIn
		body, err := readAll(r)
		if err != nil {
			errJSON(w, 400, "read body: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		var sent map[string]json.RawMessage
		_ = json.Unmarshal(body, &sent)
		if _, ok := sent["name"]; ok {
			m.Name = in.Name
		}
		if _, ok := sent["enabled"]; ok && in.Enabled != nil {
			m.Enabled = *in.Enabled
		}
		if _, ok := sent["metric"]; ok {
			m.Metric = in.Metric
		}
		if _, ok := sent["operator"]; ok {
			op, _ := monitor.CanonicalOperator(in.Operator)
			m.Operator = op
		}
		if _, ok := sent["threshold"]; ok && in.Threshold != nil {
			m.Threshold = *in.Threshold
		}
		if _, ok := sent["window_seconds"]; ok && in.WindowSeconds != nil {
			m.WindowSeconds = *in.WindowSeconds
		}
		if _, ok := sent["channel_type"]; ok {
			m.ChannelType = in.ChannelType
		}
		if _, ok := sent["channel_url"]; ok {
			m.ChannelURL = in.ChannelURL
		}
		if _, ok := sent["channel_secret"]; ok {
			m.ChannelSecret = in.ChannelSecret
		}
		check := monitorIn{
			Name: m.Name, Metric: m.Metric, Operator: m.Operator, Threshold: &m.Threshold,
			WindowSeconds: &m.WindowSeconds, ChannelType: m.ChannelType, ChannelURL: m.ChannelURL,
		}
		if msg := validateMonitor(check); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		if err := s.monitors.Save(r.Context(), m); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, monitorJSON(m))
	case http.MethodDelete:
		if err := s.monitors.Delete(r.Context(), projectID, monitorID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": monitorID})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// handleMonitorEvaluate checks one monitor now instead of waiting for the
// schedule. It is what someone clicks after changing a threshold.
func (s *Server) handleMonitorEvaluate(w http.ResponseWriter, r *http.Request, projectID, monitorID string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	ev, err := s.monitors.Evaluate(r.Context(), projectID, monitorID, s.now())
	if err == sql.ErrNoRows {
		errJSON(w, 404, "monitor not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	out := map[string]interface{}{
		"monitor": monitorJSON(ev.Monitor), "value": ev.Value,
		"firing": ev.Firing, "changed": ev.Changed,
	}
	if ev.Event != nil {
		out["event"] = monitorEventJSON(*ev.Event)
	}
	if ev.Reason != "" {
		out["reason"] = ev.Reason
	}
	writeJSON(w, 200, out)
}

// handleMonitorEvents lists the firing episodes of a project's monitors.
func (s *Server) handleMonitorEvents(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := s.monitors.Events(r.Context(), projectID, limit)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		out = append(out, monitorEventJSON(e))
	}
	writeJSON(w, 200, map[string]interface{}{"events": out})
}
