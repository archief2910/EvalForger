package api

// HTTP handlers, one file per resource family. Each method binds input, calls
// the database or queue, and writes a response; protocol and lifecycle code
// live in server.go, jobs.go, auth.go and otlp.go.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"evalforge/internal/model"
)

// ---------- span ingest ----------

// traceIDForTx runs inside the caller's transaction (single-conn pool safety).
func traceIDForTx(tx *sql.Tx, parentID string) string {
	if parentID == "" {
		return ""
	}
	var tid string
	_ = tx.QueryRow("SELECT trace_id FROM spans WHERE id=?", parentID).Scan(&tid)
	return tid
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request, projectID string) {
	var in model.IngestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, "bad json: "+err.Error())
		return
	}
	inserted, tracesCreated := 0, 0
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	tx, err := s.DB.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	// --- pass 1: assign ids and resolve trace ids ---
	// A batch may arrive with children before their parent (the SDK buffers
	// spans and flushes on span end, so a child can precede its root), so the
	// trace id is resolved in a second pass instead of a single parent lookup.
	type prep struct {
		id, parent, trace string
		ev                model.SpanEvent
	}
	preps := make([]prep, 0, len(in.Events))
	index := map[string]int{}
	for _, ev := range in.Events {
		id := ev.ID
		if id == "" {
			id = newID()
		}
		index[id] = len(preps)
		preps = append(preps, prep{id: id, parent: ev.ParentID, trace: ev.TraceID, ev: ev})
	}
	for i := 0; i < len(preps)+1; i++ {
		changed := false
		for pi := range preps {
			if preps[pi].trace != "" {
				continue
			}
			if preps[pi].parent == "" {
				preps[pi].trace = preps[pi].id // root starts its own trace
				changed = true
				continue
			}
			if pj, ok := index[preps[pi].parent]; ok && preps[pj].trace != "" {
				preps[pi].trace = preps[pj].trace
				changed = true
				continue
			}
			if t := traceIDForTx(tx, preps[pi].parent); t != "" {
				preps[pi].trace = t // parent came from an earlier batch
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for pi := range preps {
		if preps[pi].trace == "" {
			preps[pi].trace = preps[pi].id // orphan span: its own trace
		}
	}

	// --- pass 2: insert ---
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, pr := range preps {
		ev := pr.ev
		id := pr.id
		traceID := pr.trace
		if ev.ParentID == "" && ev.TraceID == "" {
			tracesCreated++
		}
		start := ev.StartMS
		if start == 0 {
			start = time.Now().UnixMilli()
		}
		end := ev.EndMS
		if end == 0 {
			end = start + 100
		}
		latency := end - start
		if latency < 0 {
			latency = 0
		}
		status := "ok"
		if ev.Error != nil {
			status = "error"
		}
		modelName := ""
		if ev.Type == "llm" {
			modelName = ev.Model
		}
		pt, ct := ev.PromptTokens, ev.CompletionTokens
		reportedUsage := pt > 0 || ct > 0
		tokenSource := ""
		var ip, op float64
		if modelName != "" {
			ip, op = model.PriceFor(modelName)
			// Zero usage on an LLM span means the caller did not report it.
			// Estimate only then, and label the span accordingly.
			if pt == 0 {
				pt = model.EstimateTokens(ev.Input)
				tokenSource = model.TokenEstimated
			}
			if ct == 0 {
				ct = model.EstimateTokens(ev.Output)
				tokenSource = model.TokenEstimated
			}
			if tokenSource == "" && reportedUsage {
				tokenSource = model.TokenProvider
			}
		}
		if ev.TokenSource == model.TokenProvider || ev.TokenSource == model.TokenEstimated {
			tokenSource = ev.TokenSource
		}
		cost := float64(pt)/1000*ip + float64(ct)/1000*op
		if err := insertSpanTx(tx, projectID, model.SpanRow{
			ID: id, TraceID: traceID, ParentID: ev.ParentID, Name: ev.Name, Type: ev.Type,
			Model: modelName, Input: ev.Input, Output: ev.Output, Error: ev.Error, Metadata: ev.Metadata,
			StartMS: start, EndMS: end, LatencyMS: latency,
			PromptTokens: pt, CompletionTokens: ct, CostUSD: cost, Status: status,
			TokenSource: tokenSource,
		}, now); err != nil {
			errJSON(w, 500, "span insert: "+err.Error())
			return
		}
		inserted++
	}
	// roll up root latency from children + propagate child errors
	rollupTraceTx(tx)

	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"inserted": inserted, "traces_created": tracesCreated})
}

// ---------- trace queries ----------

type traceRow struct {
	Input, Output, Error, Metadata, Model interface{}
	TraceID, Name, Status                 string
	LatencyMS                             int64
	CostUSD                               float64
	SpanCount                             int
	StartMS                               int64
}

// A trace's model and cost live on its LLM child spans, not on the root task
// span, so both are aggregated over the whole trace.
const traceCostExpr = `(SELECT COALESCE(SUM(c.cost_usd),0) FROM spans c WHERE c.trace_id=s.trace_id)`
const traceModelsExpr = `(SELECT GROUP_CONCAT(DISTINCT ms.model) FROM spans ms WHERE ms.trace_id=s.trace_id AND ms.model IS NOT NULL AND ms.model<>'')`

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request, projectID string) {
	q := r.URL.Query()
	where := []string{"s.project_id=?", "s.parent_id IS NULL"}
	args := []interface{}{projectID}
	if v := q.Get("status"); v != "" {
		where = append(where, "s.status=?")
		args = append(args, v)
	}
	if v := q.Get("model"); v != "" {
		where = append(where, "EXISTS (SELECT 1 FROM spans ms WHERE ms.trace_id=s.trace_id AND ms.model=?)")
		args = append(args, v)
	}
	if v := q.Get("search"); v != "" {
		like := "%" + v + "%"
		where = append(where, "(s.input LIKE ? OR s.output LIKE ? OR s.name LIKE ? OR s.error LIKE ?)")
		args = append(args, like, like, like, like)
	}
	if v := q.Get("min_latency_ms"); v != "" {
		where = append(where, "s.latency_ms>=?")
		args = append(args, toF(v))
	}
	if v := q.Get("min_cost_usd"); v != "" {
		where = append(where, traceCostExpr+">=?")
		args = append(args, toF(v))
	}
	if q.Get("has_feedback") == "true" {
		where = append(where, "EXISTS (SELECT 1 FROM feedback f WHERE f.trace_id=s.trace_id)")
	}
	orderBy := "s.start_ms DESC"
	sortBy := q.Get("sort_by")
	if sortBy == "latency_ms" || sortBy == "cost_usd" || sortBy == "start_ms" {
		dir := "DESC"
		if strings.EqualFold(q.Get("sort_dir"), "asc") {
			dir = "ASC"
		}
		col := "s." + sortBy
		if sortBy == "cost_usd" {
			col = traceCostExpr
		}
		orderBy = col + " " + dir
	}
	limit, offset := 50, 0
	fmt.Sscanf(q.Get("limit"), "%d", &limit)
	fmt.Sscanf(q.Get("offset"), "%d", &offset)

	whereSQL := strings.Join(where, " AND ")
	rows, err := s.DB.Query(`SELECT s.trace_id, s.name, s.input, s.output, s.error, s.metadata,
		`+traceModelsExpr+` AS models, s.status, s.latency_ms,
		`+traceCostExpr+` AS trace_cost, s.start_ms,
		(SELECT COUNT(*) FROM spans c WHERE c.trace_id=s.trace_id) AS span_count
		FROM spans s WHERE `+whereSQL+` ORDER BY `+orderBy+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		m := map[string]interface{}{}
		var traceID, name, status string
		var input, output, errStr, metadata, models sql.NullString
		var latency, startMS int64
		var cost float64
		var spanCount int
		if err := rows.Scan(&traceID, &name, &input, &output, &errStr, &metadata, &models, &status, &latency, &cost, &startMS, &spanCount); err != nil {
			continue
		}
		m["trace_id"] = traceID
		m["name"] = name
		m["input"] = parseJSONPtr(input.String)
		m["output"] = parseJSONPtr(output.String)
		m["error"] = parseJSONPtr(errStr.String)
		m["metadata"] = parseJSONPtr(metadata.String)
		m["model"] = models.String
		m["status"] = status
		m["latency_ms"] = latency
		m["cost_usd"] = cost
		m["start_ms"] = startMS
		m["span_count"] = spanCount
		out = append(out, m)
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	total := 0
	s.DB.QueryRow(`SELECT COUNT(*) FROM spans s WHERE `+whereSQL, args...).Scan(&total)
	writeJSON(w, 200, map[string]interface{}{"traces": out, "total": total})
}

func toF(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func spanTree(root map[string]interface{}, all map[string]map[string]interface{}) []map[string]interface{} {
	kids := []map[string]interface{}{}
	for _, m := range all {
		if pid, ok := m["parent_id"].(string); ok && pid == root["id"].(string) {
			m["children"] = spanTree(m, all)
			kids = append(kids, m)
		}
	}
	sort.Slice(kids, func(i, j int) bool {
		a, _ := kids[i]["start_ms"].(int64)
		b, _ := kids[j]["start_ms"].(int64)
		return a < b
	})
	return kids
}

func (s *Server) handleTraceDetail(w http.ResponseWriter, r *http.Request, projectID, traceID string) {
	rows, err := s.DB.Query(`SELECT id, trace_id, parent_id, name, type, model, input, output, error,
		metadata, start_ms, end_ms, latency_ms, prompt_tokens, completion_tokens, cost_usd, status,
		COALESCE(token_source,'')
		FROM spans WHERE trace_id=? AND project_id=? ORDER BY start_ms`, traceID, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	all := map[string]map[string]interface{}{}
	order := []string{}
	for rows.Next() {
		var id, tid, name, typ, status string
		var parentID, model sql.NullString
		var input, output, errStr, metadata sql.NullString
		var start, end, latency int64
		var pt, ct int
		var cost float64
		var tokenSource string
		if err := rows.Scan(&id, &tid, &parentID, &name, &typ, &model, &input, &output, &errStr, &metadata, &start, &end, &latency, &pt, &ct, &cost, &status, &tokenSource); err != nil {
			continue
		}
		m := map[string]interface{}{
			"id": id, "trace_id": tid, "name": name, "type": typ, "model": model.String,
			"input": parseJSONPtr(input.String), "output": parseJSONPtr(output.String),
			"error": parseJSONPtr(errStr.String), "metadata": parseJSONPtr(metadata.String),
			"start_ms": start, "end_ms": end, "latency_ms": latency,
			"prompt_tokens": pt, "completion_tokens": ct, "cost_usd": cost, "status": status,
			"token_source": tokenSource,
		}
		if parentID.Valid {
			m["parent_id"] = parentID.String
		}
		all[id] = m
		order = append(order, id)
	}
	roots := []map[string]interface{}{}
	for _, id := range order {
		m := all[id]
		pid := ""
		if v, ok := m["parent_id"].(string); ok {
			pid = v
		}
		if pid == "" {
			m["children"] = spanTree(m, all)
			roots = append(roots, m)
		}
	}
	fb := []map[string]interface{}{}
	frows, err := s.DB.Query(`SELECT id, trace_id, span_id, kind, value, comment, created_at FROM feedback WHERE trace_id=? ORDER BY created_at DESC`, traceID)
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var fid, tid, kind, createdAt string
			var spanID, value, comment sql.NullString
			frows.Scan(&fid, &tid, &spanID, &kind, &value, &comment, &createdAt)
			fb = append(fb, map[string]interface{}{
				"id": fid, "trace_id": tid, "kind": kind, "value": parseJSONPtr(value.String),
				"comment": comment.String, "created_at": createdAt,
			})
		}
	}
	if fb == nil {
		fb = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{"trace_id": traceID, "span_tree": roots, "feedback": fb})
}

type feedbackIn struct {
	Kind    string      `json:"kind"`
	Value   interface{} `json:"value"`
	Comment string      `json:"comment"`
	SpanID  string      `json:"span_id"`
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request, projectID, traceID string) {
	var in feedbackIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	id := newID()
	val := ""
	if s, ok := in.Value.(string); ok {
		val = s
	} else if in.Value != nil {
		val = j(in.Value)
	}
	s.dbMu.Lock()
	_, err := s.DB.Exec(`INSERT INTO feedback(id, trace_id, span_id, kind, value, comment, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, traceID, nilIfEmpty(in.SpanID), in.Kind, val, in.Comment, time.Now().UTC().Format(time.RFC3339Nano))
	s.dbMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

// ---------- dashboard ----------

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	k := (float64(len(sorted)) - 1) * p
	lo := int(k)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	return sorted[lo]*(float64(hi)-k) + sorted[hi]*(k-float64(lo))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, projectID string) {
	hours := 168
	fmt.Sscanf(r.URL.Query().Get("hours"), "%d", &hours)
	since := time.Now().UnixMilli() - int64(hours)*3600*1000

	rootRows, err := s.DB.Query(`SELECT latency_ms, status, name, start_ms FROM spans
		WHERE project_id=? AND parent_id IS NULL AND start_ms>=?`, projectID, since)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type rootRowT struct {
		latency      int64
		status, name string
		startMS      int64
	}
	var rootRowsL []rootRowT
	for rootRows.Next() {
		var rr rootRowT
		rootRows.Scan(&rr.latency, &rr.status, &rr.name, &rr.startMS)
		rootRowsL = append(rootRowsL, rr)
	}
	rootRows.Close()

	allRows, err := s.DB.Query(`SELECT model, prompt_tokens, completion_tokens, cost_usd, start_ms FROM spans
		WHERE project_id=? AND start_ms>=?`, projectID, since)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type allRowT struct {
		model   string
		pt, ct  int
		cost    float64
		startMS int64
	}
	var all []allRowT
	for allRows.Next() {
		var ar allRowT
		allRows.Scan(&ar.model, &ar.pt, &ar.ct, &ar.cost, &ar.startMS)
		all = append(all, ar)
	}
	allRows.Close()

	lat := []float64{}
	bucketMS := int64(3600 * 1000)
	if hours > 72 {
		bucketMS = 24 * 3600 * 1000
	}
	type bucket struct {
		ts        int64
		requests  int
		errors    int
		p50, p95  int64
		latencies []float64
	}
	buckets := map[int64]*bucket{}
	modelCounts := map[string]int{}
	var totalTokens int
	var totalCost float64
	for _, rr := range rootRowsL {
		lat = append(lat, float64(rr.latency))
		b := int64(rr.startMS / bucketMS * bucketMS)
		if buckets[b] == nil {
			buckets[b] = &bucket{ts: b}
		}
		buckets[b].requests++
		if rr.status == "error" {
			buckets[b].errors++
		}
		buckets[b].latencies = append(buckets[b].latencies, float64(rr.latency))
	}
	for _, ar := range all {
		totalTokens += ar.pt + ar.ct
		totalCost += ar.cost
		if ar.model != "" {
			modelCounts[ar.model]++
		}
	}
	sort.Float64s(lat)
	n := len(rootRowsL)
	errors := 0
	for _, rr := range rootRowsL {
		if rr.status == "error" {
			errors++
		}
	}
	ts := []map[string]interface{}{}
	keys := []int64{}
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		b := buckets[k]
		sort.Float64s(b.latencies)
		p50 := int64(percentile(b.latencies, 0.5))
		p95 := int64(percentile(b.latencies, 0.95))
		ts = append(ts, map[string]interface{}{
			"ts": k, "requests": b.requests, "errors": b.errors,
			"latency_p50": p50, "latency_p95": p95,
		})
	}
	if ts == nil {
		ts = []map[string]interface{}{}
	}
	mb := []map[string]interface{}{}
	// stable order by count desc
	type mc struct {
		m string
		c int
	}
	var mcs []mc
	for m, c := range modelCounts {
		mcs = append(mcs, mc{m, c})
	}
	sort.Slice(mcs, func(i, j int) bool { return mcs[i].c > mcs[j].c })
	for _, x := range mcs {
		mb = append(mb, map[string]interface{}{"model": x.m, "count": x.c})
	}
	if mb == nil {
		mb = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"requests": n,
		"errors":   errors,
		"error_rate": func() float64 {
			if n == 0 {
				return 0
			}
			return float64(errors) / float64(n)
		}(),
		"latency": map[string]interface{}{
			"p50": int64(percentile(lat, 0.5)), "p95": int64(percentile(lat, 0.95)),
			"p99": int64(percentile(lat, 0.99)),
			"mean": func() int64 {
				if n == 0 {
					return 0
				}
				var s float64
				for _, v := range lat {
					s += v
				}
				return int64(s / float64(n))
			}(),
		},
		"tokens": totalTokens, "cost_usd": totalCost,
		"span_count":      len(all),
		"model_breakdown": mb,
		"timeseries":      ts,
	})
}

// ---------- projects ----------

type projectIn struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request, _ string) {
	switch r.Method {
	case http.MethodPost:
		var in projectIn
		json.NewDecoder(r.Body).Decode(&in)
		id := newID()
		s.dbMu.Lock()
		s.DB.Exec(`INSERT INTO projects(id, name, description, created_at) VALUES (?,?,?,?)`,
			id, in.Name, in.Description, time.Now().UTC().Format(time.RFC3339Nano))
		s.dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := s.DB.Query(`SELECT id, name, description, created_at FROM projects ORDER BY created_at DESC`)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		// A project-scoped credential sees exactly one project in the list —
		// filtered rather than swapped for a single object, so the response
		// shape never depends on the credential.
		scope := ""
		if p := principalFrom(r.Context()); p != nil {
			scope = p.ProjectID
		}
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, desc, created string
			rows.Scan(&id, &name, &desc, &created)
			if scope != "" && id != scope {
				continue
			}
			out = append(out, map[string]interface{}{"id": id, "name": name, "description": desc, "created_at": created})
		}
		writeJSON(w, 200, out)
	}
}

// handleProjectOne serves a single project (GET /api/v1/projects/{id}).
func (s *Server) handleProjectOne(w http.ResponseWriter, r *http.Request, _ string, pid string) {
	var id, name, desc, created string
	err := s.DB.QueryRow(`SELECT id, name, description, created_at FROM projects WHERE id=?`, pid).
		Scan(&id, &name, &desc, &created)
	if err != nil {
		errJSON(w, 404, "project not found")
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "name": name, "description": desc, "created_at": created,
	})
}

// ---------- datasets ----------

type recordIn struct {
	Input    interface{} `json:"input"`
	Expected interface{} `json:"expected"`
	Metadata interface{} `json:"metadata"`
	Tags     []string    `json:"tags"`
}

func (s *Server) handleDatasets(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodPost:
		var in projectIn
		json.NewDecoder(r.Body).Decode(&in)
		id := newID()
		s.dbMu.Lock()
		tx, err := s.DB.BeginTx(r.Context(), nil)
		if err != nil {
			s.dbMu.Unlock()
			errJSON(w, 500, "could not create dataset")
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO datasets(id, project_id, name, description, version, created_at) VALUES (?,?,?,?,1,?)`,
			id, projectID, in.Name, in.Description, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			s.dbMu.Unlock()
			errJSON(w, 500, "could not create dataset")
			return
		}
		// Version 1 is a real, immutable snapshot (empty for a new dataset), so
		// an experiment can pin it before the first record is added.
		if _, err := s.snapshotDatasetTx(r.Context(), tx, id); err != nil {
			s.dbMu.Unlock()
			s.log.Error("snapshot new dataset", "dataset_id", id, "err", err)
			errJSON(w, 500, "could not snapshot dataset")
			return
		}
		if err := tx.Commit(); err != nil {
			s.dbMu.Unlock()
			errJSON(w, 500, "could not create dataset")
			return
		}
		s.dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := s.DB.Query(`SELECT d.id, d.name, d.description, d.version, d.created_at,
			(SELECT COUNT(*) FROM dataset_records r WHERE r.dataset_id=d.id) AS record_count
			FROM datasets d WHERE d.project_id=? ORDER BY d.created_at DESC`, projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, desc, created string
			var version, count int
			rows.Scan(&id, &name, &desc, &version, &created, &count)
			out = append(out, map[string]interface{}{"id": id, "name": name, "description": desc, "version": version, "created_at": created, "record_count": count})
		}
		writeJSON(w, 200, out)
	}
}

func (s *Server) handleDatasetOne(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	var id, projectID, name, desc, created string
	var version int
	err := s.DB.QueryRow(`SELECT id, project_id, name, description, version, created_at FROM datasets WHERE id=?`, dsID).
		Scan(&id, &projectID, &name, &desc, &version, &created)
	if err != nil || !allowProject(r, projectID) {
		errJSON(w, 404, "dataset not found")
		return
	}
	var projectName string
	s.DB.QueryRow(`SELECT name FROM projects WHERE id=?`, projectID).Scan(&projectName)
	records := []map[string]interface{}{}
	rows, err := s.DB.Query(`SELECT id, input, expected, metadata, tags, source, created_at FROM dataset_records WHERE dataset_id=? ORDER BY created_at DESC`, dsID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var rid, rinput, rsource, rcreated string
			var rexpected, rmeta, rtags sql.NullString
			rows.Scan(&rid, &rinput, &rexpected, &rmeta, &rtags, &rsource, &rcreated)
			rec := map[string]interface{}{
				"id": rid, "input": parseJSONPtr(rinput), "expected": parseJSONPtr(rexpected.String),
				"metadata": parseJSONPtr(rmeta.String), "source": rsource, "created_at": rcreated,
			}
			if rtags.String != "" {
				var tags []string
				json.Unmarshal([]byte(rtags.String), &tags)
				rec["tags"] = tags
			} else {
				rec["tags"] = []string{}
			}
			records = append(records, rec)
		}
	}
	if records == nil {
		records = []map[string]interface{}{}
	}
	var latestVersionID, latestHash string
	var latestSeq int
	_ = s.DB.QueryRow(`SELECT id, version_hash, seq FROM dataset_versions
		WHERE dataset_id=? ORDER BY seq DESC LIMIT 1`, dsID).
		Scan(&latestVersionID, &latestHash, &latestSeq)
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "project_id": projectID, "name": name, "description": desc,
		"version": version, "created_at": created, "project_name": projectName,
		"records": records, "latest_version_id": latestVersionID,
		"latest_version_hash": latestHash, "latest_version_seq": latestSeq,
	})
}

// handleAddRecord accepts either a single record object or an array of records.
// A batch is inserted in one transaction and bumps the dataset version once.
func (s *Server) handleAddRecord(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	var dsProject string
	if err := s.DB.QueryRow(`SELECT project_id FROM datasets WHERE id=?`, dsID).Scan(&dsProject); err != nil || !allowProject(r, dsProject) {
		errJSON(w, 404, "dataset not found")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		errJSON(w, 400, "read body: "+err.Error())
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		errJSON(w, 400, "empty body")
		return
	}
	var records []recordIn
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &records); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
	} else {
		var one recordIn
		if err := json.Unmarshal(trimmed, &one); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		records = []recordIn{one}
	}
	if len(records) == 0 {
		writeJSON(w, 200, map[string]interface{}{"ids": []string{}})
		return
	}
	ids := make([]string, 0, len(records))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.dbMu.Lock()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, "could not add records")
		return
	}
	defer tx.Rollback()
	for _, in := range records {
		id := newID()
		rectags := in.Tags
		if rectags == nil {
			rectags = []string{}
		}
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO dataset_records(id, dataset_id, input, expected, metadata, tags, source, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			id, dsID, j(in.Input), jsonOrNull(in.Expected), jsonOrNull(in.Metadata), j(rectags), "manual", now); err != nil {
			s.dbMu.Unlock()
			s.log.Error("add dataset record", "dataset_id", dsID, "err", err)
			errJSON(w, 500, "could not add dataset record")
			return
		}
		ids = append(ids, id)
	}
	// Snapshot in the same transaction as the rows: a reader can never observe
	// records that no version describes.
	if _, err := s.snapshotDatasetTx(r.Context(), tx, dsID); err != nil {
		s.dbMu.Unlock()
		s.log.Error("snapshot dataset", "dataset_id", dsID, "err", err)
		errJSON(w, 500, "could not snapshot dataset")
		return
	}
	if err := tx.Commit(); err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, "could not add dataset records")
		return
	}
	s.dbMu.Unlock()
	if len(ids) == 1 {
		writeJSON(w, 200, map[string]string{"id": ids[0]})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ids": ids})
}

func (s *Server) handleRecordFromTrace(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	var in struct {
		TraceID  string      `json:"trace_id"`
		Expected interface{} `json:"expected"`
		Tags     []string    `json:"tags"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	// find project + root input from trace
	var projectID, rootInput string
	err := s.DB.QueryRow(`SELECT s.project_id, s.input FROM spans s
		JOIN datasets d ON d.project_id = s.project_id
		WHERE s.trace_id=? AND s.parent_id IS NULL LIMIT 1`, in.TraceID).Scan(&projectID, &rootInput)
	if err != nil || !allowProject(r, projectID) {
		errJSON(w, 404, "trace not found")
		return
	}
	id := newID()
	tags := in.Tags
	if tags == nil {
		tags = []string{"from-trace"}
	}
	meta := map[string]string{"origin_trace_id": in.TraceID, "source": "review"}
	s.dbMu.Lock()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, "could not promote trace")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO dataset_records(id, dataset_id, input, expected, metadata, tags, source, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		id, dsID, rootInput, jsonOrNull(in.Expected), j(meta), j(tags), "review",
		time.Now().UTC().Format(time.RFC3339Nano)); err == nil {
		_, err = s.snapshotDatasetTx(r.Context(), tx, dsID)
	}
	if err == nil {
		err = tx.Commit()
	}
	s.dbMu.Unlock()
	if err != nil {
		s.log.Error("promote trace to dataset", "dataset_id", dsID, "err", err)
		errJSON(w, 500, "could not promote trace")
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

// ---------- prompts ----------

func (s *Server) handlePrompts(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Name     string      `json:"name"`
			Model    string      `json:"model"`
			Template interface{} `json:"template"`
			Params   interface{} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		var ver int
		s.DB.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM prompts WHERE project_id=? AND name=?`, projectID, in.Name).Scan(&ver)
		id := newID()
		s.dbMu.Lock()
		s.DB.Exec(`INSERT INTO prompts(id, project_id, name, version, model, template, params, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, projectID, in.Name, ver, in.Model, j(in.Template), j(in.Params), time.Now().UTC().Format(time.RFC3339Nano))
		s.dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := s.DB.Query(`SELECT id, name, version, model, template, params, created_at FROM prompts WHERE project_id=? ORDER BY name, version DESC`, projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, created string
			var version int
			var model, template, params sql.NullString
			rows.Scan(&id, &name, &version, &model, &template, &params, &created)
			out = append(out, map[string]interface{}{
				"id": id, "name": name, "version": version, "model": model.String,
				"template": parseJSONPtr(template.String), "params": parseJSONPtr(params.String), "created_at": created,
			})
		}
		writeJSON(w, 200, out)
	}
}

// ---------- scorers (registry served by Go; execution in worker) ----------

var builtinScorers = []map[string]interface{}{
	{"name": "exact_match", "kind": "deterministic", "description": "Case-insensitive exact match against expected"},
	{"name": "contains", "kind": "deterministic", "description": "Expected substring present in output"},
	{"name": "levenshtein_similarity", "kind": "deterministic", "description": "1 - edit distance / max length"},
	{"name": "json_valid", "kind": "deterministic", "description": "Output parses as valid JSON"},
	{"name": "numeric_diff", "kind": "deterministic", "description": "Numeric closeness of first number in output vs expected"},
	{"name": "regex_guard", "kind": "deterministic", "description": "Pattern guard (must_match / must_not_match), e.g. no PII"},
	{"name": "factuality_judge", "kind": "llm_judge", "description": "LLM-as-judge: factual correctness vs expected"},
	{"name": "helpfulness_judge", "kind": "llm_judge", "description": "LLM-as-judge: helpfulness of the response"},
}

func (s *Server) handleScorers(w http.ResponseWriter, r *http.Request, _ ...string) {
	writeJSON(w, 200, builtinScorers)
}

// ---------- evaluations (Go dispatches, Python worker runs) ----------

func (s *Server) handleStartEval(w http.ResponseWriter, r *http.Request, projectID string) {
	var in model.EvalInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		errJSON(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.DatasetID) == "" {
		errJSON(w, 400, "dataset_id is required")
		return
	}
	if s.queue == nil {
		errJSON(w, 503, "evaluation dispatcher is not running")
		return
	}

	expID := newID()
	// The experiment, its pinned dataset snapshot and its job are one
	// transaction: a crash cannot create an experiment nobody owns, and a
	// rerun cannot silently read different dataset rows.
	s.dbMu.Lock()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, "begin evaluation")
		return
	}
	defer tx.Rollback()

	var datasetProject string
	if err := tx.QueryRowContext(r.Context(),
		`SELECT project_id FROM datasets WHERE id=?`, in.DatasetID).Scan(&datasetProject); err != nil || datasetProject != projectID {
		s.dbMu.Unlock()
		errJSON(w, 404, "dataset not found in this project")
		return
	}
	versionID, err := s.datasetVersionForEvalTx(r.Context(), tx, in.DatasetID, in.DatasetVersionID)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 400, err.Error())
		return
	}
	prompt, err := s.resolvePromptForEvalTx(r.Context(), tx, projectID, in)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 404, err.Error())
		return
	}
	in.DatasetVersionID = versionID
	if prompt != nil {
		in.PromptVersionID = prompt.ID
		in.PromptName = prompt.Name
		in.PromptVersion = prompt.Version
	}

	_, err = tx.ExecContext(r.Context(), `INSERT INTO experiments(id, project_id, name, dataset_id, baseline_experiment_id,
		status, dataset_version_id, prompt_version_id, created_at)
		VALUES (?,?,?,?,?,'running',?,?,?)`,
		expID, projectID, in.ExperimentName, in.DatasetID, nilIfEmpty(in.BaselineExperimentID),
		versionID, nilIfEmpty(in.PromptVersionID), model.NowUTC())
	if err == nil {
		var payload []byte
		payload, err = json.Marshal(in)
		if err == nil {
			err = s.queue.EnqueueEvalTx(r.Context(), tx, expID, projectID, payload)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	s.dbMu.Unlock()
	if err != nil {
		s.log.Error("create evaluation", "err", err)
		errJSON(w, 500, "could not create evaluation")
		return
	}
	// The dispatcher claims it within one tick; the request only enqueues.
	writeJSON(w, 200, map[string]interface{}{
		"experiment_id":      expID,
		"dataset_version_id": versionID,
		"prompt_version_id":  in.PromptVersionID,
	})
}

func (s *Server) handleExperimentDetail(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	var id, projectID, name, datasetID, status, created string
	var baseline, datasetVersionID, promptVersionID sql.NullString
	var versionHash, promptName string
	var versionSeq, promptVersion int
	var summary sql.NullString
	err := s.DB.QueryRow(`SELECT e.id, e.project_id, e.name, e.dataset_id, e.baseline_experiment_id, e.status, e.summary, e.created_at,
		COALESCE(e.dataset_version_id,''), COALESCE(e.prompt_version_id,''),
		COALESCE((SELECT seq FROM dataset_versions v WHERE v.id=e.dataset_version_id),0),
		COALESCE((SELECT version_hash FROM dataset_versions v WHERE v.id=e.dataset_version_id),''),
		COALESCE((SELECT name FROM prompts p WHERE p.id=e.prompt_version_id),''),
		COALESCE((SELECT version FROM prompts p WHERE p.id=e.prompt_version_id),0)
		FROM experiments e WHERE e.id=?`, expID).Scan(&id, &projectID, &name, &datasetID, &baseline, &status, &summary, &created,
		&datasetVersionID, &promptVersionID, &versionSeq, &versionHash, &promptName, &promptVersion)
	if err != nil {
		errJSON(w, 404, "experiment not found")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "experiment not found")
		return
	}
	var datasetName string
	s.DB.QueryRow(`SELECT name FROM datasets WHERE id=?`, datasetID).Scan(&datasetName)
	runs := []map[string]interface{}{}
	rows, err := s.DB.Query(`SELECT id, record_id, input, expected, output, error, latency_ms, scores, output_mode, created_at
		FROM eval_runs WHERE experiment_id=? ORDER BY created_at`, expID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var rid, recID, rinput, rcreated string
			var rexpected, routput, rerror, rscores, rmode sql.NullString
			var lat int
			rows.Scan(&rid, &recID, &rinput, &rexpected, &routput, &rerror, &lat, &rscores, &rmode, &rcreated)
			runs = append(runs, map[string]interface{}{
				"id": rid, "record_id": recID, "input": parseJSONPtr(rinput), "expected": parseJSONPtr(rexpected.String),
				"output": parseJSONPtr(routput.String), "error": parseJSONPtr(rerror.String),
				"latency_ms": lat, "scores": parseJSONPtr(rscores.String), "output_mode": rmode.String,
				"created_at": rcreated,
			})
		}
	}
	if runs == nil {
		runs = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "project_id": projectID, "name": name, "dataset_id": datasetID,
		"dataset_name":           datasetName,
		"baseline_experiment_id": baseline.String, "status": status,
		"dataset_version_id": datasetVersionID.String, "prompt_version_id": promptVersionID.String,
		"dataset_version_seq": versionSeq, "dataset_version_hash": versionHash,
		"prompt_name": promptName, "prompt_version": promptVersion,
		"summary": parseJSONPtr(summary.String), "created_at": created, "runs": runs,
	})
}

func (s *Server) handleExperimentsList(w http.ResponseWriter, r *http.Request, projectID string) {
	rows, err := s.DB.Query(`SELECT e.id, e.name, e.dataset_id, e.baseline_experiment_id, e.status, e.summary, e.created_at,
		COALESCE(e.dataset_version_id,''), COALESCE(e.prompt_version_id,''),
		d.name AS dataset_name,
		(SELECT COUNT(*) FROM eval_runs r WHERE r.experiment_id=e.id) AS run_count
		FROM experiments e LEFT JOIN datasets d ON d.id=e.dataset_id
		WHERE e.project_id=? ORDER BY e.created_at DESC`, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, name, datasetID, status, created, datasetName string
		var datasetVersionID, promptVersionID string
		var baseline, summary sql.NullString
		var runCount int
		rows.Scan(&id, &name, &datasetID, &baseline, &status, &summary, &created,
			&datasetVersionID, &promptVersionID, &datasetName, &runCount)
		out = append(out, map[string]interface{}{
			"id": id, "name": name, "dataset_id": datasetID, "dataset_name": datasetName,
			"baseline_experiment_id": baseline.String, "status": status,
			"dataset_version_id": datasetVersionID, "prompt_version_id": promptVersionID,
			"summary": parseJSONPtr(summary.String), "created_at": created, "run_count": runCount,
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	var expProject string
	if err := s.DB.QueryRow(`SELECT project_id FROM experiments WHERE id=?`, expID).Scan(&expProject); err != nil || !allowProject(r, expProject) {
		errJSON(w, 404, "experiment not found")
		return
	}
	baselineID := r.URL.Query().Get("baseline_id")
	base := map[string]map[string]interface{}{}
	rows, err := s.DB.Query(`SELECT input, output, expected, scores FROM eval_runs WHERE experiment_id=?`, baselineID)
	if err == nil {
		for rows.Next() {
			var input, output, expected, scores string
			rows.Scan(&input, &output, &expected, &scores)
			key := input
			base[key] = map[string]interface{}{
				"output": parseJSONPtr(output), "expected": parseJSONPtr(expected), "scores": parseJSONPtr(scores),
			}
		}
		rows.Close()
	}
	rows, err = s.DB.Query(`SELECT input, output, expected, scores, error, latency_ms, output_mode FROM eval_runs WHERE experiment_id=?`, expID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var input, output, expected, scores string
		var errStr, omode sql.NullString
		var lat int
		rows.Scan(&input, &output, &expected, &scores, &errStr, &lat, &omode)
		sc := parseJSONPtr(scores)
		delta := map[string]interface{}{}
		baseScores := map[string]interface{}{}
		if b, ok := base[input]; ok {
			if bs, ok := b["scores"].(map[string]interface{}); ok {
				baseScores = bs
			}
		}
		if sm, ok := sc.(map[string]interface{}); ok {
			for name, sv := range sm {
				if svm, ok := sv.(map[string]interface{}); ok {
					ns, _ := svm["score"].(float64)
					bv := 0.0
					if bsm, ok := baseScores[name].(map[string]interface{}); ok {
						if bf, ok := bsm["score"].(float64); ok {
							bv = bf
						}
					}
					delta[name] = ns - bv
				}
			}
		}
		row := map[string]interface{}{
			"input": parseJSONPtr(input), "output": parseJSONPtr(output), "expected": parseJSONPtr(expected),
			"scores": sc, "baseline_output": nil, "baseline_scores": nil, "delta": delta,
			"error": parseJSONPtr(errStr.String), "latency_ms": lat, "output_mode": omode.String,
		}
		if b, ok := base[input]; ok {
			row["baseline_output"] = b["output"]
			row["baseline_scores"] = b["scores"]
		}
		out = append(out, row)
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	writeJSON(w, 200, out)
}

// ---------- worker: execute eval runs in Python ----------
// The worker generates task outputs and computes scores, then writes eval_runs + summary
// over an HTTP callback (see worker.py).

func (s *Server) handleWorkerCallback(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	var in struct {
		Runs []struct {
			RecordID   string                 `json:"record_id"`
			Input      interface{}            `json:"input"`
			Expected   interface{}            `json:"expected"`
			Output     interface{}            `json:"output"`
			Error      interface{}            `json:"error"`
			LatencyMS  int                    `json:"latency_ms"`
			Scores     map[string]interface{} `json:"scores"`
			OutputMode string                 `json:"output_mode"`
		} `json:"runs"`
		Summary map[string]interface{} `json:"summary"`
		Status  string                 `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.dbMu.Lock()
	tx, err := s.DB.Begin()
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	// The callback is idempotent: a re-dispatched or retried worker must replace
	// its experiment's runs instead of appending a second copy.
	if _, err := tx.Exec(`DELETE FROM eval_runs WHERE experiment_id=?`, expID); err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	for _, run := range in.Runs {
		tx.Exec(`INSERT INTO eval_runs(id, experiment_id, record_id, input, expected, output, error, latency_ms, scores, output_mode, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			newID(), expID, nilIfEmpty(run.RecordID), j(run.Input), jsonOrNull(run.Expected),
			jsonOrNull(run.Output), jsonOrNull(run.Error), run.LatencyMS, j(run.Scores),
			run.OutputMode, time.Now().UTC().Format(time.RFC3339Nano))
	}
	tx.Exec(`UPDATE experiments SET summary=?, status=? WHERE id=?`, j(in.Summary), in.Status, expID)
	if err := tx.Commit(); err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	s.dbMu.Unlock()
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// ---- health ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request, _ ...string) {
	ok := s.DB.Ping() == nil
	jobs := map[string]int{}
	if s.queue != nil {
		jobs = s.queue.Counts()
	}
	writeJSON(w, 200, map[string]interface{}{"ok": ok, "jobs": jobs})
}
