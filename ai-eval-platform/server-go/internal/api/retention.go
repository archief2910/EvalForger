package api

// Retention policy API.
//
// The rule is control-plane state; the telemetry it governs is not touched by
// editing it. `POST /retention-rules/{id}/apply` runs one rule immediately,
// which is what someone wants after setting a policy on a project that
// already holds a lot of data.

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"

	"evalforge/internal/retention"
)

type retentionIn struct {
	Name          string `json:"name"`
	RetentionDays *int   `json:"retention_days"`
	ApplyToPast   *bool  `json:"apply_to_past"`
	Enabled       *bool  `json:"enabled"`
}

func validateRetention(in retentionIn) string {
	if in.RetentionDays == nil {
		return "retention_days is required"
	}
	if *in.RetentionDays <= 0 {
		return "retention_days must be a positive number of days"
	}
	if *in.RetentionDays > 3650 {
		return "retention_days must be 3650 or fewer"
	}
	if len(in.Name) > 120 {
		return "name is too long (max 120)"
	}
	return ""
}

func retentionJSON(r retention.Rule) map[string]interface{} {
	return map[string]interface{}{
		"id": r.ID, "project_id": r.ProjectID, "name": r.Name,
		"retention_days": r.RetentionDays, "apply_to_past": r.ApplyToPast,
		"enabled": r.Enabled, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
	}
}

func (s *Server) handleRetentionRules(w http.ResponseWriter, r *http.Request, projectID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := s.retention.List(r.Context(), projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(rules))
		for _, rule := range rules {
			out = append(out, retentionJSON(rule))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in retentionIn
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		if msg := validateRetention(in); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		rule := retention.Rule{
			ProjectID: projectID, Name: in.Name,
			RetentionDays: *in.RetentionDays,
			ApplyToPast:   true,
			Enabled:       true,
		}
		if in.ApplyToPast != nil {
			rule.ApplyToPast = *in.ApplyToPast
		}
		if in.Enabled != nil {
			rule.Enabled = *in.Enabled
		}
		if err := s.retention.Insert(r.Context(), &rule); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, retentionJSON(rule))
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func (s *Server) handleRetentionRule(w http.ResponseWriter, r *http.Request, projectID, ruleID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	rule, err := s.retention.Get(r.Context(), projectID, ruleID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "retention rule not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, retentionJSON(rule))
	case http.MethodPatch, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			errJSON(w, 400, "read body: "+err.Error())
			return
		}
		var in retentionIn
		if err := json.Unmarshal(body, &in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		var sent map[string]json.RawMessage
		_ = json.Unmarshal(body, &sent)
		if _, ok := sent["name"]; ok {
			rule.Name = in.Name
		}
		if _, ok := sent["retention_days"]; ok && in.RetentionDays != nil {
			rule.RetentionDays = *in.RetentionDays
		}
		if _, ok := sent["apply_to_past"]; ok && in.ApplyToPast != nil {
			rule.ApplyToPast = *in.ApplyToPast
		}
		if _, ok := sent["enabled"]; ok && in.Enabled != nil {
			rule.Enabled = *in.Enabled
		}
		check := retentionIn{RetentionDays: &rule.RetentionDays, Name: rule.Name}
		if msg := validateRetention(check); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		if err := s.retention.Save(r.Context(), rule); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, retentionJSON(rule))
	case http.MethodDelete:
		if err := s.retention.Delete(r.Context(), projectID, ruleID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": ruleID})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// handleRetentionApply runs one rule right now, instead of waiting for the
// scheduled sweep.
func (s *Server) handleRetentionApply(w http.ResponseWriter, r *http.Request, projectID, ruleID string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	rule, err := s.retention.Get(r.Context(), projectID, ruleID)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "retention rule not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	res, err := s.retention.Apply(r.Context(), rule)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"applied": res})
}

// retentionSummary feeds the dashboard: how much telemetry each project keeps
// and whether a policy is set.
func (s *Server) retentionSummary(r *http.Request, projectID string) map[string]interface{} {
	out := map[string]interface{}{"rules": 0, "retention_days": nil, "enabled": false}
	rules, err := s.retention.List(r.Context(), projectID)
	if err != nil {
		return out
	}
	out["rules"] = len(rules)
	for _, rule := range rules {
		if rule.Enabled {
			out["enabled"] = true
			out["retention_days"] = rule.RetentionDays
			break
		}
	}
	return out
}
