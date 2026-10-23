package api

// Dataset versioning and prompt pinning.
//
// A dataset is mutable; an experiment must not be. Every write to a dataset
// therefore snapshots the full item set into dataset_versions, and experiments
// pin that row at creation. The worker reads the snapshot, never the live
// table. This is the Opik model at SQLite scale: full snapshots instead of
// deltas, with the JSON column as the migration seam.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"net/http"

	"evalforge/internal/model"
)

const datasetRecordColumns = `id, input, expected, metadata, tags, source, created_at`

// datasetVersionForEvalTx returns the version an experiment should pin. An
// explicit id must belong to the dataset; otherwise the latest snapshot is
// used, and a dataset that predates versioning gets one created lazily.
func (s *Server) datasetVersionForEvalTx(ctx context.Context, tx *sql.Tx, datasetID, requestedID string) (string, error) {
	if requestedID != "" {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dataset_versions WHERE id=? AND dataset_id=?`,
			requestedID, datasetID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return "", fmt.Errorf("dataset_version_id %s does not belong to dataset %s", requestedID, datasetID)
		}
		return requestedID, nil
	}
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM dataset_versions WHERE dataset_id=? ORDER BY seq DESC LIMIT 1`, datasetID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return s.snapshotDatasetTx(ctx, tx, datasetID)
}

// snapshotDatasetTx writes an immutable version if the current content differs
// from the latest one. It runs inside the caller's write transaction.
func (s *Server) snapshotDatasetTx(ctx context.Context, tx *sql.Tx, datasetID string) (string, error) {
	records, err := readDatasetRecordsTx(ctx, tx, datasetID)
	if err != nil {
		return "", err
	}
	items, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(items)
	hash := hex.EncodeToString(sum[:])

	var (
		prevID, prevHash, prevJSON string
		prevSeq                    int
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, version_hash, items_json, seq FROM dataset_versions
		 WHERE dataset_id=? ORDER BY seq DESC LIMIT 1`, datasetID).
		Scan(&prevID, &prevHash, &prevJSON, &prevSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil && prevHash == hash {
		return prevID, nil
	}

	added, modified, deleted := diffDatasetRecords(prevJSON, records)
	seq := prevSeq + 1
	if errors.Is(err, sql.ErrNoRows) {
		// First snapshot: continue the legacy integer counter rather than
		// resetting a long-lived dataset to version 1.
		var current int
		if scanErr := tx.QueryRowContext(ctx, `SELECT version FROM datasets WHERE id=?`, datasetID).Scan(&current); scanErr == nil && current > seq {
			seq = current
		}
	}
	id := newID()
	_, err = tx.ExecContext(ctx, `INSERT INTO dataset_versions(
		id, dataset_id, seq, version_hash, items_json, items_total,
		items_added, items_modified, items_deleted, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, datasetID, seq, hash, string(items), len(records), added, modified, deleted, model.NowUTC())
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE datasets SET version=? WHERE id=?`, seq, datasetID); err != nil {
		return "", err
	}
	return id, nil
}

func readDatasetRecordsTx(ctx context.Context, tx *sql.Tx, datasetID string) ([]model.DatasetRecord, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+datasetRecordColumns+` FROM dataset_records WHERE dataset_id=? ORDER BY created_at, id`, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.DatasetRecord, 0)
	for rows.Next() {
		var (
			id, input, source, created string
			expected, metadata, tags   sql.NullString
		)
		if err := rows.Scan(&id, &input, &expected, &metadata, &tags, &source, &created); err != nil {
			return nil, err
		}
		rec := model.DatasetRecord{
			ID: id, Input: parseJSONPtr(input), Expected: parseJSONPtr(expected.String),
			Metadata: parseJSONPtr(metadata.String), Source: source, CreatedAt: created,
			Tags: []string{},
		}
		if tags.String != "" {
			_ = json.Unmarshal([]byte(tags.String), &rec.Tags)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// diffDatasetRecords computes the delta counts Opik reports for a version.
func diffDatasetRecords(prevJSON string, current []model.DatasetRecord) (added, modified, deleted int) {
	prev := map[string]string{}
	if prevJSON != "" {
		var old []model.DatasetRecord
		if json.Unmarshal([]byte(prevJSON), &old) == nil {
			for _, r := range old {
				b, _ := json.Marshal(r)
				prev[r.ID] = string(b)
			}
		}
	}
	seen := make(map[string]bool, len(current))
	for _, r := range current {
		seen[r.ID] = true
		b, _ := json.Marshal(r)
		old, ok := prev[r.ID]
		if !ok {
			added++
		} else if old != string(b) {
			modified++
		}
	}
	for id := range prev {
		if !seen[id] {
			deleted++
		}
	}
	return added, modified, deleted
}

// resolvePromptForEvalTx resolves either an explicit pinned prompt id (used by
// reruns) or a name/version reference chosen at experiment creation.
func (s *Server) resolvePromptForEvalTx(ctx context.Context, tx *sql.Tx, projectID string, in model.EvalInput) (*model.Prompt, error) {
	if in.PromptVersionID != "" && strings.TrimSpace(in.PromptName) == "" {
		return s.resolvePromptByIDTx(ctx, tx, projectID, in.PromptVersionID)
	}
	return s.resolvePromptTx(ctx, tx, projectID, in.PromptName, in.PromptVersion)
}

// resolvePromptByIDTx loads one prompt version, scoped to the project so a
// crafted rerun cannot pin another project's prompt.
func (s *Server) resolvePromptByIDTx(ctx context.Context, tx *sql.Tx, projectID, promptID string) (*model.Prompt, error) {
	var (
		p                           model.Prompt
		modelName, template, params sql.NullString
	)
	err := tx.QueryRowContext(ctx, `SELECT id, project_id, name, version, model, template, params, created_at
		FROM prompts WHERE id=? AND project_id=?`, promptID, projectID).
		Scan(&p.ID, &p.ProjectID, &p.Name, &p.Version, &modelName, &template, &params, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("prompt version %s not found in this project", promptID)
	}
	if err != nil {
		return nil, err
	}
	p.Model = modelName.String
	p.Template = parseJSONPtr(template.String)
	p.Params = parseJSONPtr(params.String)
	return &p, nil
}

// resolvePromptTx resolves an optional prompt reference. Version 0 means the
// latest version of that name.
func (s *Server) resolvePromptTx(ctx context.Context, tx *sql.Tx, projectID, name string, version int) (*model.Prompt, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	var (
		p                           model.Prompt
		modelName, template, params sql.NullString
	)
	query := `SELECT id, project_id, name, version, model, template, params, created_at
		FROM prompts WHERE project_id=? AND name=?`
	args := []interface{}{projectID, name}
	if version > 0 {
		query += ` AND version=?`
		args = append(args, version)
	}
	query += ` ORDER BY version DESC LIMIT 1`
	if err := tx.QueryRowContext(ctx, query, args...).Scan(
		&p.ID, &p.ProjectID, &p.Name, &p.Version, &modelName, &template, &params, &p.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if version > 0 {
				return nil, fmt.Errorf("prompt %q version %d not found", name, version)
			}
			return nil, fmt.Errorf("prompt %q not found", name)
		}
		return nil, err
	}
	p.Model = modelName.String
	p.Template = parseJSONPtr(template.String)
	p.Params = parseJSONPtr(params.String)
	return &p, nil
}

// ---------- read endpoints used by the worker and the UI ----------

func (s *Server) handleDatasetVersion(w http.ResponseWriter, r *http.Request, _ string, versionID string) {
	var (
		id, datasetID, hash, items, created  string
		seq, total, added, modified, deleted int
	)
	err := s.DB.QueryRow(`SELECT id, dataset_id, seq, version_hash, items_json, items_total,
		items_added, items_modified, items_deleted, created_at
		FROM dataset_versions WHERE id=?`, versionID).
		Scan(&id, &datasetID, &seq, &hash, &items, &total, &added, &modified, &deleted, &created)
	if err != nil {
		errJSON(w, 404, "dataset version not found")
		return
	}
	var versionProject string
	if err := s.DB.QueryRow(`SELECT project_id FROM datasets WHERE id=?`, datasetID).Scan(&versionProject); err != nil || !allowProject(r, versionProject) {
		errJSON(w, 404, "dataset version not found")
		return
	}
	var records []model.DatasetRecord
	_ = json.Unmarshal([]byte(items), &records)
	if records == nil {
		records = []model.DatasetRecord{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "dataset_id": datasetID, "seq": seq, "version_hash": hash,
		"items_total": total, "items_added": added, "items_modified": modified,
		"items_deleted": deleted, "created_at": created, "records": records,
	})
}

func (s *Server) handleDatasetVersions(w http.ResponseWriter, r *http.Request, _ string, datasetID string) {
	var dsProject string
	if err := s.DB.QueryRow(`SELECT project_id FROM datasets WHERE id=?`, datasetID).Scan(&dsProject); err != nil || !allowProject(r, dsProject) {
		errJSON(w, 404, "dataset not found")
		return
	}
	rows, err := s.DB.Query(`SELECT id, seq, version_hash, items_total, items_added, items_modified,
		items_deleted, created_at FROM dataset_versions WHERE dataset_id=? ORDER BY seq DESC`, datasetID)
	if err != nil {
		errJSON(w, 500, "could not list dataset versions")
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var (
			id, hash, created               string
			seq, total, added, mod, deleted int
		)
		if rows.Scan(&id, &seq, &hash, &total, &added, &mod, &deleted, &created) == nil {
			out = append(out, map[string]interface{}{
				"id": id, "seq": seq, "version_hash": hash, "items_total": total,
				"items_added": added, "items_modified": mod, "items_deleted": deleted,
				"created_at": created,
			})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePromptVersion(w http.ResponseWriter, r *http.Request, _ string, promptID string) {
	var (
		id, projectID, name, created string
		version                      int
		modelName, template, params  sql.NullString
	)
	err := s.DB.QueryRow(`SELECT id, project_id, name, version, model, template, params, created_at
		FROM prompts WHERE id=?`, promptID).
		Scan(&id, &projectID, &name, &version, &modelName, &template, &params, &created)
	if err != nil || !allowProject(r, projectID) {
		errJSON(w, 404, "prompt version not found")
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "project_id": projectID, "name": name, "version": version,
		"model": modelName.String, "template": parseJSONPtr(template.String),
		"params": parseJSONPtr(params.String), "created_at": created,
	})
}
