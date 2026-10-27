package api

// The optimization loop: evaluate, propose a better prompt, re-evaluate, keep
// the winner.
//
// This is the one feature here that creates an artifact rather than a
// measurement. It pins its inputs at creation (a dataset snapshot, a baseline
// prompt version, a scorer list) so a run is reproducible, and it writes the
// winner as a new prompt version only if it actually scored better — a search
// that always "wins" is not a search.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"evalforge/internal/model"
	"evalforge/internal/queue"
	"evalforge/internal/worker"
)

type optimizationRow struct {
	ID                      string
	ProjectID               string
	Name                    string
	DatasetID               string
	DatasetVersionID        string
	BaselinePromptVersionID string
	BestPromptVersionID     string
	Scorers                 []string
	MaxCandidates           int
	Status                  string
	BaselineScore           sql.NullFloat64
	BestScore               sql.NullFloat64
	Summary                 string
	Error                   string
	CreatedAt               string
	UpdatedAt               string
}

const optimizationColumns = `id, project_id, name, dataset_id, dataset_version_id,
	baseline_prompt_version_id, best_prompt_version_id, scorers, max_candidates, status,
	baseline_score, best_score, summary, error, created_at, updated_at`

func scanOptimization(sc interface{ Scan(...interface{}) error }) (optimizationRow, error) {
	var o optimizationRow
	var scorers string
	err := sc.Scan(&o.ID, &o.ProjectID, &o.Name, &o.DatasetID, &o.DatasetVersionID,
		&o.BaselinePromptVersionID, &o.BestPromptVersionID, &scorers, &o.MaxCandidates, &o.Status,
		&o.BaselineScore, &o.BestScore, &o.Summary, &o.Error, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return optimizationRow{}, err
	}
	if json.Unmarshal([]byte(scorers), &o.Scorers) != nil {
		o.Scorers = []string{}
	}
	return o, nil
}

func optimizationJSON(o optimizationRow) map[string]interface{} {
	out := map[string]interface{}{
		"id": o.ID, "project_id": o.ProjectID, "name": o.Name,
		"dataset_id": o.DatasetID, "dataset_version_id": o.DatasetVersionID,
		"baseline_prompt_version_id": o.BaselinePromptVersionID,
		"best_prompt_version_id":     o.BestPromptVersionID,
		"scorers":                    o.Scorers, "max_candidates": o.MaxCandidates,
		"status": o.Status, "created_at": o.CreatedAt, "updated_at": o.UpdatedAt,
	}
	if o.BaselineScore.Valid {
		out["baseline_score"] = o.BaselineScore.Float64
	}
	if o.BestScore.Valid {
		out["best_score"] = o.BestScore.Float64
	}
	if o.Error != "" {
		out["error"] = o.Error
	}
	if o.Summary != "" {
		var summary interface{}
		if json.Unmarshal([]byte(o.Summary), &summary) == nil {
			out["summary"] = summary
		}
	}
	return out
}

func (s *Server) handleOptimizations(w http.ResponseWriter, r *http.Request, projectID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.DB.Query(`SELECT `+optimizationColumns+` FROM optimizations
			WHERE project_id=? ORDER BY created_at DESC LIMIT 100`, projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			o, err := scanOptimization(rows)
			if err != nil {
				continue
			}
			out = append(out, optimizationJSON(o))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			Name             string   `json:"name"`
			DatasetID        string   `json:"dataset_id"`
			DatasetVersionID string   `json:"dataset_version_id"`
			PromptVersionID  string   `json:"prompt_version_id"`
			Scorers          []string `json:"scorers"`
			MaxCandidates    int      `json:"max_candidates"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		if s.queue == nil {
			errJSON(w, 503, "evaluation dispatcher is not running")
			return
		}
		datasetID, versionID, promptID, msg := s.resolveOptimizationInputs(r.Context(), projectID, in.DatasetID, in.DatasetVersionID, in.PromptVersionID)
		if msg != "" {
			errJSON(w, 400, msg)
			return
		}
		if len(in.Scorers) == 0 {
			in.Scorers = []string{"exact_match"}
		}
		known := map[string]bool{}
		for _, sc := range builtinScorers {
			known[sc["name"].(string)] = true
		}
		for _, name := range in.Scorers {
			if !known[name] {
				errJSON(w, 400, "unknown scorer: "+name)
				return
			}
		}
		if in.MaxCandidates == 0 {
			in.MaxCandidates = 4
		}
		if in.MaxCandidates < 1 || in.MaxCandidates > 8 {
			errJSON(w, 400, "max_candidates must be between 1 and 8")
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = "optimize " + time.Now().UTC().Format("2006-01-02 15:04")
		}
		now := model.NowUTC()
		id := model.NewID()
		scorersJSON, _ := json.Marshal(in.Scorers)
		payload, _ := json.Marshal(map[string]interface{}{
			"dataset_version_id": versionID, "prompt_version_id": promptID,
			"scorers": in.Scorers, "max_candidates": in.MaxCandidates,
		})

		s.dbMu.Lock()
		tx, err := s.DB.BeginTx(r.Context(), nil)
		if err == nil {
			if _, err = tx.ExecContext(r.Context(),
				`INSERT INTO optimizations(id, project_id, name, dataset_id, dataset_version_id,
				 baseline_prompt_version_id, scorers, max_candidates, status, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,'queued',?,?)`,
				id, projectID, name, datasetID, versionID, promptID, string(scorersJSON),
				in.MaxCandidates, now, now); err == nil {
				_, err = s.queue.EnqueueOptimizeTx(r.Context(), tx, id, projectID, payload)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		s.dbMu.Unlock()
		if err != nil {
			errJSON(w, 500, "could not create optimization: "+err.Error())
			return
		}
		o, _ := s.getOptimization(r.Context(), projectID, id)
		writeJSON(w, 200, optimizationJSON(o))
	default:
		errJSON(w, 405, "method not allowed")
	}
}

// resolveOptimizationInputs pins the dataset snapshot and prompt version, and
// reports the first thing that is wrong with them.
func (s *Server) resolveOptimizationInputs(ctx context.Context, projectID, datasetID, versionID, promptID string) (string, string, string, string) {
	if datasetID == "" {
		return "", "", "", "dataset_id is required"
	}
	var dsProject string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT project_id FROM datasets WHERE id=?`, datasetID).Scan(&dsProject); err != nil {
		return "", "", "", "dataset not found"
	}
	if dsProject != projectID {
		return "", "", "", "dataset not found"
	}
	if versionID == "" {
		// Pin the latest snapshot by sequence: a version id is what the run
		// stores, and the sequence is what "latest" means for this dataset.
		_ = s.DB.QueryRowContext(ctx,
			`SELECT id FROM dataset_versions WHERE dataset_id=? ORDER BY seq DESC LIMIT 1`,
			datasetID).Scan(&versionID)
	}
	if versionID == "" {
		return "", "", "", "dataset has no snapshot yet"
	}
	if promptID != "" {
		var promptProject string
		if err := s.DB.QueryRowContext(ctx,
			`SELECT project_id FROM prompts WHERE id=?`, promptID).Scan(&promptProject); err != nil {
			return "", "", "", "prompt version not found"
		}
		if promptProject != projectID {
			return "", "", "", "prompt version not found"
		}
	}
	return datasetID, versionID, promptID, ""
}

func (s *Server) getOptimization(ctx context.Context, projectID, id string) (optimizationRow, error) {
	return scanOptimization(s.DB.QueryRowContext(ctx,
		`SELECT `+optimizationColumns+` FROM optimizations WHERE id=? AND project_id=?`, id, projectID))
}

func (s *Server) handleOptimization(w http.ResponseWriter, r *http.Request, projectID, id string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	if r.Method != http.MethodGet {
		errJSON(w, 405, "method not allowed")
		return
	}
	o, err := s.getOptimization(r.Context(), projectID, id)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "optimization not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	out := optimizationJSON(o)
	// Resolve the winning prompt text so a reader can act on the result without
	// a second request.
	if o.BestPromptVersionID != "" {
		var body sql.NullString
		var version int
		if err := s.DB.QueryRowContext(r.Context(),
			`SELECT template, version FROM prompts WHERE id=?`,
			o.BestPromptVersionID).Scan(&body, &version); err == nil {
			out["best_prompt"] = map[string]interface{}{
				"version": version, "template": parseJSONPtr(body.String),
			}
		}
	}
	writeJSON(w, 200, out)
}

// runOptimizeJob executes one optimization through the worker.
func (s *Server) runOptimizeJob(ctx context.Context, j queue.Job) queue.Outcome {
	var in struct {
		DatasetVersionID string   `json:"dataset_version_id"`
		PromptVersionID  string   `json:"prompt_version_id"`
		Scorers          []string `json:"scorers"`
		MaxCandidates    int      `json:"max_candidates"`
	}
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
		return queue.Outcome{Diagnostic: "bad optimization payload: " + err.Error()}
	}
	out := s.worker.RunOptimize(ctx, worker.OptimizeRequest{
		OptimizationID: j.ExperimentID, ProjectID: j.ProjectID,
		DatasetVersionID: in.DatasetVersionID, BaselinePromptID: in.PromptVersionID,
		Scorers: in.Scorers, MaxCandidates: in.MaxCandidates,
	})
	if out.Diagnostic == "" && !out.TimedOut {
		// The worker reports over HTTP before exiting; a clean exit with no
		// callback means the run produced nothing and must not be marked done.
		var status string
		_ = s.DB.QueryRow(`SELECT status FROM optimizations WHERE id=?`, j.ExperimentID).Scan(&status)
		if status == "running" || status == "queued" {
			s.finishOptimization(j.ExperimentID, false, "worker exited without reporting results")
			return queue.Outcome{Diagnostic: "worker exited without reporting results"}
		}
	}
	if out.Diagnostic != "" {
		s.finishOptimization(j.ExperimentID, false, out.Diagnostic)
	}
	return queue.Outcome(out)
}

// finishOptimization closes an optimization run.
func (s *Server) finishOptimization(id string, ok bool, reason string) {
	now := model.NowUTC()
	if ok {
		_, _ = s.DB.Exec(`UPDATE optimizations SET status='completed', updated_at=? WHERE id=? AND status<>'completed'`,
			now, id)
		return
	}
	_, _ = s.DB.Exec(`UPDATE optimizations SET status='failed', error=?, updated_at=? WHERE id=? AND status<>'completed'`,
		reason, now, id)
}

// handleOptimizationCallback receives the worker's result.
//
// The winner becomes a new prompt version only when it beat the baseline. That
// is the whole point of the loop: a candidate that ties or loses is reported
// and discarded, never promoted.
func (s *Server) handleOptimizationCallback(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Baseline struct {
			Prompt json.RawMessage    `json:"prompt"`
			Scores map[string]float64 `json:"scores"`
			Mean   *float64           `json:"mean"`
		} `json:"baseline"`
		Candidates []struct {
			Name   string             `json:"name"`
			Prompt json.RawMessage    `json:"prompt"`
			Scores map[string]float64 `json:"scores"`
			Mean   *float64           `json:"mean"`
		} `json:"candidates"`
		Best struct {
			Name   string             `json:"name"`
			Prompt json.RawMessage    `json:"prompt"`
			Scores map[string]float64 `json:"scores"`
			Mean   *float64           `json:"mean"`
		} `json:"best"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	var projectID, name, baselinePromptID string
	err := s.DB.QueryRow(`SELECT project_id, name, baseline_prompt_version_id FROM optimizations WHERE id=?`, id).
		Scan(&projectID, &name, &baselinePromptID)
	if err != nil {
		errJSON(w, 404, "optimization not found")
		return
	}
	if in.Error != "" {
		s.finishOptimization(id, false, in.Error)
		writeJSON(w, 200, map[string]interface{}{"ok": false, "error": in.Error})
		return
	}

	bestMean := 0.0
	if in.Best.Mean != nil {
		bestMean = *in.Best.Mean
	}
	baselineMean := 0.0
	hasBaseline := in.Baseline.Mean != nil
	if hasBaseline {
		baselineMean = *in.Baseline.Mean
	}
	improved := !hasBaseline || bestMean > baselineMean
	bestPromptVersionID := ""
	promoted := false

	// Promote only an improvement, and only when the worker actually produced a
	// prompt for the winner.
	if improved && len(in.Best.Prompt) > 0 {
		versionID, err := s.createOptimizedPrompt(r.Context(), projectID, name, string(in.Best.Prompt))
		if err != nil {
			s.finishOptimization(id, false, "could not save the winning prompt: "+err.Error())
			errJSON(w, 500, "could not save the winning prompt")
			return
		}
		bestPromptVersionID = versionID
		promoted = true
	}

	summary, _ := json.Marshal(map[string]interface{}{
		"baseline":      in.Baseline,
		"candidates":    in.Candidates,
		"best":          in.Best,
		"improved":      improved,
		"promoted":      promoted,
		"baseline_mean": in.Baseline.Mean,
		"best_mean":     in.Best.Mean,
	})
	now := model.NowUTC()
	_, err = s.DB.Exec(`UPDATE optimizations SET
			status='completed', baseline_score=?, best_score=?, best_prompt_version_id=?,
			summary=?, error='', updated_at=? WHERE id=?`,
		nullableFloat(in.Baseline.Mean), nullableFloat(in.Best.Mean), bestPromptVersionID,
		string(summary), now, id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "improved": improved, "promoted": promoted,
		"best_prompt_version_id": bestPromptVersionID,
	})
}

// createOptimizedPrompt appends the winning prompt as a new version. Prompt
// versions are rows in `prompts` numbered per (project, name), and the number is
// derived from the current maximum inside one transaction so two runs cannot
// claim the same version.
//
// The body arrives already JSON-encoded, either as a quoted string (a plain
// template) or as an object (a message list). Storing it verbatim keeps the
// shape the worker produced, which is what `parseJSONPtr` gives back on read.
func (s *Server) createOptimizedPrompt(ctx context.Context, projectID, optimizationName, template string) (string, error) {
	name := "optimized: " + optimizationName
	now := model.NowUTC()
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0)+1 FROM prompts WHERE project_id=? AND name=?`,
		projectID, name).Scan(&version); err != nil {
		return "", err
	}
	versionID := model.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prompts(id, project_id, name, version, model, template, params, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		versionID, projectID, name, version, "", template, "", now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return versionID, nil
}

func nullableFloat(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// optimizationStats feeds the dashboard block.
func (s *Server) optimizationStats(projectID string) map[string]interface{} {
	out := map[string]interface{}{"total": 0, "completed": 0, "promoted": 0, "best_gain": nil}
	var total, completed, promoted int
	var bestGain sql.NullFloat64
	_ = s.DB.QueryRow(`SELECT COUNT(*),
			COUNT(*) FILTER (WHERE status='completed'),
			COUNT(*) FILTER (WHERE best_prompt_version_id<>'')
		FROM optimizations WHERE project_id=?`, projectID).Scan(&total, &completed, &promoted)
	out["total"], out["completed"], out["promoted"] = total, completed, promoted
	_ = s.DB.QueryRow(`SELECT MAX(best_score - baseline_score) FROM optimizations
		WHERE project_id=? AND baseline_score IS NOT NULL AND best_score IS NOT NULL`, projectID).
		Scan(&bestGain)
	if bestGain.Valid {
		out["best_gain"] = bestGain.Float64
	}
	return out
}

var _ = fmt.Sprintf
