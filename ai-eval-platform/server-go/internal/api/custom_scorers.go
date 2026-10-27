package api

// Custom (user-authored) metrics.
//
// A custom metric is arbitrary Python. It is stored with its author, it is
// listed in the scorer registry alongside the builtins, and it is only ever
// executed inside the sandbox. If the sandbox is not reachable, the metric
// does not run: the API says so, the evaluation records the failure, and
// nothing falls back to executing it on the host.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"evalforge/internal/model"
	"evalforge/internal/sandbox"
)

// customScorerRow is one registered metric.
type customScorerRow struct {
	ID          string
	ProjectID   string
	Name        string
	Description string
	Code        string
	CreatedAt   string
	UpdatedAt   string
}

// metricName is the shape a metric name must have: it is used as an identifier
// in rules and in the registry, so it cannot contain anything that would make
// it ambiguous there.
var metricName = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// maxMetricCode bounds what a metric can be. A metric is a function, not an
// application; a megabyte of Python is a sign the boundary is being abused.
const maxMetricCode = 64 << 10

// validateMetricCode checks a submitted metric without executing it. Syntax
// is checked by the sandbox on first run; what is checked here is the shape
// that would let a metric escape its contract (empty code, no score function).
func validateMetricCode(code string) string {
	if strings.TrimSpace(code) == "" {
		return "code is required"
	}
	if len(code) > maxMetricCode {
		return "code is too large (max 64 KiB)"
	}
	// A metric must define something to call. The runner accepts either a
	// score function or a class with a score method, so look for both.
	if !strings.Contains(code, "def score") {
		return "code must define a score(payload) function or a class with a score method"
	}
	return ""
}

func (s *Server) listCustomScorers(ctx context.Context, projectID string) ([]customScorerRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, project_id, name, description, code, created_at, updated_at
		 FROM custom_scorers WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []customScorerRow{}
	for rows.Next() {
		var r customScorerRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.Description, &r.Code, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// customScorerByName resolves one metric for the evaluation path.
func (s *Server) customScorerByName(ctx context.Context, projectID, name string) (customScorerRow, error) {
	var r customScorerRow
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, project_id, name, description, code, created_at, updated_at
		 FROM custom_scorers WHERE project_id=? AND name=?`, projectID, name).
		Scan(&r.ID, &r.ProjectID, &r.Name, &r.Description, &r.Code, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func customScorerJSON(r customScorerRow) map[string]interface{} {
	return map[string]interface{}{
		"id": r.ID, "project_id": r.ProjectID, "name": r.Name,
		"description": r.Description, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
		"scope": "record", "kind": "python_metric",
		"sandbox": sSandboxDescription(),
	}
}

// sSandboxDescription names the isolation backend for API consumers. It is a
// package-level hook so the JSON helper does not need the Server.
var sandboxDescriber func() string

func sSandboxDescription() string {
	if sandboxDescriber == nil {
		return "docker"
	}
	return sandboxDescriber()
}

func (s *Server) handleCustomScorers(w http.ResponseWriter, r *http.Request, projectID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.listCustomScorers(r.Context(), projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(rows))
		for _, row := range rows {
			item := customScorerJSON(row)
			item["code"] = row.Code
			out = append(out, item)
		}
		writeJSON(w, 200, map[string]interface{}{
			"scorers": out, "sandbox_available": s.sandbox.Available(),
		})
	case http.MethodPost:
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Code        string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetricCode+1<<10)).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if !metricName.MatchString(in.Name) {
			errJSON(w, 400, "name must be lowercase letters, digits and underscores (3-64 chars)")
			return
		}
		if msg := validateMetricCode(in.Code); msg != "" {
			errJSON(w, 400, msg)
			return
		}
		now := model.NowUTC()
		row := customScorerRow{
			ID: model.NewID(), ProjectID: projectID, Name: in.Name,
			Description: in.Description, Code: in.Code, CreatedAt: now, UpdatedAt: now,
		}
		_, err := s.DB.ExecContext(r.Context(),
			`INSERT INTO custom_scorers(id, project_id, name, description, code, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?)`,
			row.ID, row.ProjectID, row.Name, row.Description, row.Code, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			// The unique index is the authority on duplicate names, so a race
			// between two registrations surfaces as a 409, not a 500.
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") ||
				strings.Contains(strings.ToLower(err.Error()), "unique") {
				errJSON(w, 409, "a metric with that name already exists in this project")
				return
			}
			errJSON(w, 500, err.Error())
			return
		}
		item := customScorerJSON(row)
		item["code"] = row.Code
		writeJSON(w, 200, item)
	default:
		errJSON(w, 405, "method not allowed")
	}
}

func (s *Server) handleCustomScorer(w http.ResponseWriter, r *http.Request, projectID, name string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	row, err := s.customScorerByName(r.Context(), projectID, name)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "custom metric not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		item := customScorerJSON(row)
		item["code"] = row.Code
		writeJSON(w, 200, item)
	case http.MethodDelete:
		if _, err := s.DB.ExecContext(r.Context(),
			`DELETE FROM custom_scorers WHERE id=? AND project_id=?`, row.ID, projectID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"deleted": name})
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// handleCustomScorerTest runs a metric once against a sample payload. It is the
// only way to find out whether the code works without waiting for a rule to
// fire, and it uses the same sandbox the evaluation path uses.
func (s *Server) handleCustomScorerTest(w http.ResponseWriter, r *http.Request, projectID, name string) {
	if r.Method != http.MethodPost {
		errJSON(w, 405, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	row, err := s.customScorerByName(r.Context(), projectID, name)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "custom metric not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var in struct {
		Payload map[string]interface{} `json:"payload"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in)
	if in.Payload == nil {
		in.Payload = map[string]interface{}{
			"input": "What is your refund policy?", "output": "Within 30 days.", "expected": "",
		}
	}
	if !s.sandbox.Available() {
		errJSON(w, 503, "sandbox is unavailable; custom metrics do not run without isolation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := s.sandbox.Run(ctx, row.Code, in.Payload)
	if err != nil {
		status := 422
		if errors.Is(err, sandbox.ErrUnavailable) {
			status = 503
		}
		writeJSON(w, status, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "score": res.Score, "rationale": res.Rationale,
		"sandbox": s.sandbox.Describe(),
	})
}

// customScorerNames returns the project's metric names, for the scorer
// registry and for rule validation.
func (s *Server) customScorerNames(ctx context.Context, projectID string) map[string]bool {
	out := map[string]bool{}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name FROM custom_scorers WHERE project_id=?`, projectID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out[name] = true
		}
	}
	return out
}
