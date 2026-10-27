package api

// First-class conversations (threads).
//
// A thread is a conversation: a sequence of traces ("turns") that share a
// thread id. Ingest upserts the thread and marks it open; an idle sweeper
// closes it after THREAD_IDLE_SECONDS; any new activity reopens it. Scores can
// target the conversation as a whole, which is what makes multi-turn quality
// visible instead of judging each turn in isolation.

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
)

const defaultThreadScorers = "thread_completion,thread_error_free,thread_turn_consistency,thread_coherence_judge,thread_context_retention_judge"

// syncThreadTx recomputes one thread row from its spans. It is called after
// ingest, inside the telemetry write transaction, so the thread row can never
// disagree with the spans that created it.
func syncThreadTx(ctx context.Context, tx *sql.Tx, projectID, threadID string, now string) error {
	if threadID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO threads(
			project_id, id, status, started_at_ms, last_activity_ms, ended_at_ms,
			trace_count, created_at, updated_at)
		SELECT project_id, thread_id, 'open', MIN(start_ms), MAX(COALESCE(end_ms,start_ms)), NULL,
			COUNT(DISTINCT trace_id), ?, ?
		FROM spans WHERE project_id=? AND thread_id=?
		GROUP BY project_id, thread_id
		ON CONFLICT DO NOTHING`, now, now, projectID, threadID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE threads SET
			started_at_ms = (SELECT MIN(s.start_ms) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id),
			last_activity_ms = (SELECT MAX(COALESCE(s.end_ms,s.start_ms)) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id),
			trace_count = (SELECT COUNT(DISTINCT s.trace_id) FROM spans s
				WHERE s.project_id=threads.project_id AND s.thread_id=threads.id),
			status='open', ended_at_ms=NULL, updated_at=?
		WHERE project_id=? AND id=?
		  AND EXISTS (SELECT 1 FROM spans s WHERE s.project_id=threads.project_id AND s.thread_id=threads.id)`,
		now, projectID, threadID)
	return err
}

// propagateThreadTx gives every span of a trace the trace's conversation id.
// A trace is one turn, so a thread id supplied on one span applies to all of
// them, including spans that arrived in an earlier batch.
func propagateThreadTx(ctx context.Context, tx *sql.Tx, projectID, traceID, threadID string) error {
	if traceID == "" || threadID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE spans SET thread_id=?
		WHERE project_id=? AND trace_id=? AND (thread_id IS NULL OR thread_id='')`,
		threadID, projectID, traceID)
	return err
}

// assignTraceThreadTx rewrites the thread of every span in a trace. It is used
// when a caller supplies an explicit conversation id: a trace cannot be split
// across conversations, even if an earlier batch used the trace-id fallback.
func assignTraceThreadTx(ctx context.Context, tx *sql.Tx, projectID, traceID, threadID string) error {
	if traceID == "" || threadID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE spans SET thread_id=?
		WHERE project_id=? AND trace_id=?`, threadID, projectID, traceID)
	return err
}

// pruneThreadsTx removes threads whose spans were all reassigned to another
// conversation, along with their now-orphaned scores.
func pruneThreadsTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM thread_scores
		WHERE project_id=? AND NOT EXISTS (
			SELECT 1 FROM threads t WHERE t.project_id=thread_scores.project_id AND t.id=thread_scores.thread_id)`,
		projectID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM threads
		WHERE project_id=? AND NOT EXISTS (
			SELECT 1 FROM spans s WHERE s.project_id=threads.project_id AND s.thread_id=threads.id)`,
		projectID)
	return err
}

// existingThreadTx returns a thread already associated with a trace, so a later
// batch of the same turn keeps its conversation even when it omits thread_id.
func existingThreadTx(tx *sql.Tx, projectID, traceID string) string {
	if traceID == "" {
		return ""
	}
	var id string
	_ = tx.QueryRow(`SELECT thread_id FROM spans
		WHERE project_id=? AND trace_id=? AND thread_id IS NOT NULL AND thread_id<>''
		LIMIT 1`, projectID, traceID).Scan(&id)
	return id
}

// ---------- list / detail ----------

type threadListRow struct {
	ID             string
	Status         string
	StartedAtMS    int64
	LastActivityMS int64
	EndedAtMS      sql.NullInt64
	TraceCount     int
	AvgScore       sql.NullFloat64
	ScoreCount     int
	LastScoreAt    sql.NullString
	LastInput      sql.NullString
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	q := r.URL.Query()
	where := []string{"t.project_id=?"}
	args := []interface{}{projectID}
	if v := q.Get("status"); v != "" {
		where = append(where, "t.status=?")
		args = append(args, v)
	}
	if v := q.Get("search"); v != "" {
		like := "%" + v + "%"
		where = append(where, `(t.id LIKE ? OR EXISTS (
			SELECT 1 FROM spans s WHERE s.project_id=t.project_id AND s.thread_id=t.id
			  AND (s.input LIKE ? OR s.output LIKE ? OR s.name LIKE ?)))`)
		args = append(args, like, like, like, like)
	}
	if v := q.Get("hours"); v != "" {
		var hours int
		fmt.Sscanf(v, "%d", &hours)
		if hours > 0 {
			where = append(where, "t.last_activity_ms>=?")
			args = append(args, time.Now().UnixMilli()-int64(hours)*3600*1000)
		}
	}
	orderBy := "t.last_activity_ms DESC"
	switch q.Get("sort_by") {
	case "started_at_ms":
		orderBy = "t.started_at_ms DESC"
	case "trace_count":
		orderBy = "t.trace_count DESC"
	case "avg_score":
		orderBy = "avg_score DESC"
	case "last_activity_ms", "":
		orderBy = "t.last_activity_ms DESC"
	}
	limit, offset := 100, 0
	fmt.Sscanf(q.Get("limit"), "%d", &limit)
	fmt.Sscanf(q.Get("offset"), "%d", &offset)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	whereSQL := strings.Join(where, " AND ")
	base := `SELECT t.id, t.status, t.started_at_ms, t.last_activity_ms, t.ended_at_ms, t.trace_count,
		(SELECT AVG(ts.score) FROM thread_scores ts WHERE ts.project_id=t.project_id
			AND ts.thread_id=t.id AND ts.score IS NOT NULL) AS avg_score,
		(SELECT COUNT(ts.score) FROM thread_scores ts WHERE ts.project_id=t.project_id
			AND ts.thread_id=t.id AND ts.score IS NOT NULL) AS score_count,
		(SELECT MAX(ts.created_at) FROM thread_scores ts WHERE ts.project_id=t.project_id
			AND ts.thread_id=t.id) AS last_score_at,
		(SELECT s.input FROM spans s WHERE s.project_id=t.project_id AND s.thread_id=t.id
			AND s.parent_id IS NULL ORDER BY s.start_ms DESC LIMIT 1) AS last_input
		FROM threads t WHERE ` + whereSQL

	rows, err := s.Telemetry.Query(base+" ORDER BY "+orderBy+" LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var t threadListRow
		if err := rows.Scan(&t.ID, &t.Status, &t.StartedAtMS, &t.LastActivityMS, &t.EndedAtMS,
			&t.TraceCount, &t.AvgScore, &t.ScoreCount, &t.LastScoreAt, &t.LastInput); err != nil {
			continue
		}
		row := map[string]interface{}{
			"id": t.ID, "project_id": projectID, "status": t.Status, "started_at_ms": t.StartedAtMS,
			"last_activity_ms": t.LastActivityMS, "trace_count": t.TraceCount,
			"turn_count": t.TraceCount, "score_count": t.ScoreCount,
			"duration_ms": t.LastActivityMS - t.StartedAtMS,
			"last_input":  parseJSONPtr(t.LastInput.String),
		}
		if t.EndedAtMS.Valid {
			row["ended_at_ms"] = t.EndedAtMS.Int64
		}
		if t.AvgScore.Valid {
			row["avg_score"] = t.AvgScore.Float64
		}
		if t.LastScoreAt.Valid {
			row["last_score_at"] = t.LastScoreAt.String
		}
		out = append(out, row)
	}
	var total int
	_ = s.Telemetry.QueryRow(`SELECT COUNT(*) FROM threads t WHERE `+whereSQL, args...).Scan(&total)
	writeJSON(w, 200, map[string]interface{}{"threads": out, "total": total})
}

func (s *Server) handleThreadDetail(w http.ResponseWriter, r *http.Request, projectID, threadID string) {
	if r.Method != http.MethodGet {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	thread, err := s.threadRow(projectID, threadID)
	if err != nil {
		errJSON(w, 404, "thread not found")
		return
	}

	turns := []map[string]interface{}{}
	rows, err := s.Telemetry.Query(`SELECT s.trace_id, s.name, s.input, s.output, s.error, s.status,
		s.latency_ms, s.start_ms,
		(SELECT COALESCE(SUM(c.cost_usd),0) FROM spans c
			WHERE c.project_id=s.project_id AND c.trace_id=s.trace_id) AS trace_cost,
		(SELECT COUNT(*) FROM spans c WHERE c.project_id=s.project_id AND c.trace_id=s.trace_id) AS span_count
		FROM spans s
		WHERE s.project_id=? AND s.thread_id=? AND s.parent_id IS NULL
		  AND s.id = (SELECT s2.id FROM spans s2 WHERE s2.project_id=s.project_id
			AND s2.trace_id=s.trace_id AND s2.parent_id IS NULL ORDER BY s2.start_ms LIMIT 1)
		ORDER BY s.start_ms`, projectID, threadID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var traceID, name, status string
			var input, output, errStr sql.NullString
			var latency, startMS int64
			var cost float64
			var spanCount int
			if rows.Scan(&traceID, &name, &input, &output, &errStr, &status, &latency, &startMS, &cost, &spanCount) != nil {
				continue
			}
			turns = append(turns, map[string]interface{}{
				"trace_id": traceID, "name": name, "input": parseJSONPtr(input.String),
				"output": parseJSONPtr(output.String), "error": parseJSONPtr(errStr.String),
				"status": status, "latency_ms": latency, "start_ms": startMS,
				"cost_usd": cost, "span_count": spanCount,
			})
		}
	}

	scores, summary := s.threadScores(projectID, threadID)
	evals := []map[string]interface{}{}
	jrows, err := s.DB.Query(`SELECT id, status, attempts, last_error, created_at, updated_at
		FROM jobs WHERE kind=? AND project_id=? AND thread_id=? ORDER BY created_at DESC LIMIT 20`,
		queue.KindThreadEval, projectID, threadID)
	if err == nil {
		defer jrows.Close()
		for jrows.Next() {
			var id, status, created, updated string
			var attempts int
			var lastError sql.NullString
			if jrows.Scan(&id, &status, &attempts, &lastError, &created, &updated) != nil {
				continue
			}
			evals = append(evals, map[string]interface{}{
				"job_id": id, "status": status, "attempts": attempts,
				"last_error": lastError.String, "created_at": created, "updated_at": updated,
			})
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"thread": thread, "turns": turns, "scores": scores,
		"score_summary": summary, "evaluations": evals,
		"online_scores": s.onlineThreadScoresForThread(r.Context(), projectID, threadID),
	})
}

// onlineThreadScoresForThread returns the automatic scores the live rule loop
// produced for a conversation. They are kept apart from the evaluation
// scores because they answer a different question: what did the rule say
// about this conversation when it closed.
func (s *Server) onlineThreadScoresForThread(ctx context.Context, projectID, threadID string) []map[string]interface{} {
	out := []map[string]interface{}{}
	rows, err := s.Telemetry.QueryContext(ctx,
		`SELECT rule_id, scorer, score, rationale, judge_mode, created_at
		 FROM online_thread_scores WHERE project_id=? AND thread_id=? ORDER BY created_at`,
		projectID, threadID)
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

func (s *Server) threadRow(projectID, threadID string) (map[string]interface{}, error) {
	var id, status, created, updated string
	var started, last int64
	var ended sql.NullInt64
	var traceCount int
	var metadata sql.NullString
	err := s.Telemetry.QueryRow(`SELECT id, status, started_at_ms, last_activity_ms, ended_at_ms,
		trace_count, COALESCE(metadata,''), created_at, updated_at
		FROM threads WHERE project_id=? AND id=?`, projectID, threadID).
		Scan(&id, &status, &started, &last, &ended, &traceCount, &metadata, &created, &updated)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"id": id, "project_id": projectID, "status": status, "started_at_ms": started,
		"last_activity_ms": last, "trace_count": traceCount, "turn_count": traceCount,
		"duration_ms": last - started, "created_at": created, "updated_at": updated,
	}
	if ended.Valid {
		out["ended_at_ms"] = ended.Int64
	}
	if metadata.String != "" {
		out["metadata"] = parseJSONPtr(metadata.String)
	}
	return out, nil
}

func (s *Server) threadScores(projectID, threadID string) ([]map[string]interface{}, map[string]interface{}) {
	out := []map[string]interface{}{}
	rows, err := s.Telemetry.Query(`SELECT id, COALESCE(job_id,''), name, score, COALESCE(value,''), rationale,
		source, COALESCE(judge_mode,''), created_at
		FROM thread_scores WHERE project_id=? AND thread_id=? ORDER BY created_at DESC`, projectID, threadID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, jobID, name, value, rationale, source, judgeMode, created string
			var score sql.NullFloat64
			if rows.Scan(&id, &jobID, &name, &score, &value, &rationale, &source, &judgeMode, &created) != nil {
				continue
			}
			row := map[string]interface{}{
				"id": id, "job_id": jobID, "name": name, "value": parseJSONPtr(value),
				"rationale": rationale, "source": source, "judge_mode": judgeMode, "created_at": created,
			}
			if score.Valid {
				row["score"] = score.Float64
			}
			out = append(out, row)
		}
	}
	summary := map[string]interface{}{}
	type acc struct {
		sum   float64
		count int
		last  string
	}
	byName := map[string]*acc{}
	for _, row := range out {
		name, _ := row["name"].(string)
		score, ok := row["score"].(float64)
		if !ok {
			continue
		}
		if byName[name] == nil {
			byName[name] = &acc{}
		}
		a := byName[name]
		a.sum += score
		a.count++
		if created, _ := row["created_at"].(string); created > a.last {
			a.last = created
		}
	}
	for name, a := range byName {
		summary[name] = map[string]interface{}{
			"avg": round4(a.sum / float64(a.count)), "count": a.count, "last": a.last,
		}
	}
	return out, summary
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// ---------- lifecycle ----------

func (s *Server) handleThreadStatus(w http.ResponseWriter, r *http.Request, projectID, threadID, action string) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	now := model.NowUTC()
	s.telemetryMu.Lock()
	var err error
	switch action {
	case "close":
		_, err = s.Telemetry.Exec(`UPDATE threads SET status='closed',
			ended_at_ms=COALESCE(ended_at_ms,last_activity_ms), updated_at=?
			WHERE project_id=? AND id=?`, now, projectID, threadID)
	case "open":
		_, err = s.Telemetry.Exec(`UPDATE threads SET status='open', ended_at_ms=NULL, updated_at=?
			WHERE project_id=? AND id=?`, now, projectID, threadID)
	default:
		err = fmt.Errorf("unknown action")
	}
	s.telemetryMu.Unlock()
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	// A manual close ends the conversation, so the online thread rules score
	// it exactly as they would score one that closed on its own.
	if action == "close" {
		s.online.AfterThreadClose(projectID, threadID)
	}
	thread, err := s.threadRow(projectID, threadID)
	if err != nil {
		errJSON(w, 404, "thread not found")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"thread": thread})
}

// closeIdleThreads closes idle conversations. The interval is derived from the
// configured timeout so tests and demos do not wait minutes for a sweep.
func (s *Server) sweepThreads(ctx context.Context) {
	idle := s.cfg.ThreadIdle
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	interval := idle / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	// Close conversations that went idle while the process was down before
	// waiting for the first tick.
	s.closeIdleThreads(idle)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.closeIdleThreads(idle)
		}
	}
}

// closeIdleThreads closes conversations that have gone quiet and hands each one
// to the online thread rules.
//
// The close is the trigger for conversation scoring: "how did this
// conversation go" is only answerable once the conversation is over. A rule
// that scored every turn would report a partial conversation and change its
// meaning as more turns arrive.
func (s *Server) closeIdleThreads(idle time.Duration) {
	cutoff := time.Now().Add(-idle).UnixMilli()
	now := model.NowUTC()
	s.telemetryMu.Lock()
	// Collect the conversations that are about to close, so the online rules
	// can be told which ones to score.
	rows, err := s.Telemetry.Query(`SELECT project_id, id FROM threads
		WHERE status='open' AND last_activity_ms < ?`, cutoff)
	closing := []struct{ projectID, threadID string }{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var projectID, threadID string
			if rows.Scan(&projectID, &threadID) == nil {
				closing = append(closing, struct{ projectID, threadID string }{projectID, threadID})
			}
		}
	}
	res, err := s.Telemetry.Exec(`UPDATE threads SET status='closed',
		ended_at_ms=last_activity_ms, updated_at=?
		WHERE status='open' AND last_activity_ms < ?`, now, cutoff)
	s.telemetryMu.Unlock()
	if err != nil {
		s.log.Warn("close idle threads", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.logf("closed idle threads", "count", n)
	}
	// Scoring happens after the close is durable, and off the request path.
	for _, c := range closing {
		s.online.AfterThreadClose(c.projectID, c.threadID)
	}
}

// ---------- scores ----------

func (s *Server) handleThreadScores(w http.ResponseWriter, r *http.Request, projectID, threadID string) {
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	if _, err := s.threadRow(projectID, threadID); err != nil {
		errJSON(w, 404, "thread not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		scores, summary := s.threadScores(projectID, threadID)
		writeJSON(w, 200, map[string]interface{}{"scores": scores, "score_summary": summary})
	case http.MethodPost:
		var in struct {
			Name      string      `json:"name"`
			Score     *float64    `json:"score"`
			Value     interface{} `json:"value"`
			Rationale string      `json:"rationale"`
			Source    string      `json:"source"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			errJSON(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(in.Name) == "" {
			errJSON(w, 400, "name is required")
			return
		}
		if in.Score != nil && (*in.Score < 0 || *in.Score > 1) {
			errJSON(w, 400, "score must be between 0 and 1")
			return
		}
		value := ""
		if in.Value != nil {
			if s, ok := in.Value.(string); ok {
				value = s
			} else {
				value = j(in.Value)
			}
		}
		source := in.Source
		if source == "" {
			source = "manual"
		}
		id := model.NewID()
		var score interface{}
		if in.Score != nil {
			score = *in.Score
		}
		s.telemetryMu.Lock()
		_, err := s.Telemetry.Exec(`INSERT INTO thread_scores(
			id, project_id, thread_id, job_id, name, score, value, rationale, source, judge_mode, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, projectID, threadID, nil, in.Name, score, value, in.Rationale, source, "manual", model.NowUTC())
		s.telemetryMu.Unlock()
		if err != nil {
			errJSON(w, 500, "could not save thread score")
			return
		}
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------- evaluation ----------

func (s *Server) handleThreadEvaluate(w http.ResponseWriter, r *http.Request, projectID, threadID string) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !allowProject(r, projectID) {
		errJSON(w, 404, "project not found")
		return
	}
	if s.queue == nil {
		errJSON(w, 503, "evaluation dispatcher is not running")
		return
	}
	if _, err := s.threadRow(projectID, threadID); err != nil {
		errJSON(w, 404, "thread not found")
		return
	}
	var in struct {
		Scorers []string `json:"scorers"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in)
	names := in.Scorers
	if len(names) == 0 {
		for _, n := range strings.Split(defaultThreadScorers, ",") {
			names = append(names, strings.TrimSpace(n))
		}
	}
	payload, err := json.Marshal(model.ThreadEvalInput{ThreadID: threadID, ScorerNames: names})
	if err != nil {
		errJSON(w, 500, "could not encode evaluation")
		return
	}
	s.dbMu.Lock()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.dbMu.Unlock()
		errJSON(w, 500, "could not create thread evaluation")
		return
	}
	defer tx.Rollback()
	jobID, err := s.queue.EnqueueThreadEvalTx(r.Context(), tx, projectID, threadID, payload)
	if err == nil {
		err = tx.Commit()
	}
	s.dbMu.Unlock()
	if err != nil {
		s.log.Error("create thread evaluation", "thread_id", threadID, "err", err)
		errJSON(w, 500, "could not create thread evaluation")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"job_id": jobID, "status": queue.StatusQueued})
}

// handleThreadCallback receives scores from the Python worker. The job row in
// the control plane is the authority for project/thread, so a malformed body
// cannot write scores onto someone else's conversation.
func (s *Server) handleThreadCallback(w http.ResponseWriter, r *http.Request, jobID string) {
	var in struct {
		Scores []struct {
			Name      string      `json:"name"`
			Score     *float64    `json:"score"`
			Value     interface{} `json:"value"`
			Rationale string      `json:"rationale"`
			Source    string      `json:"source"`
			JudgeMode string      `json:"judge_mode"`
		} `json:"scores"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	var kind, projectID, threadID string
	err := s.DB.QueryRow(`SELECT kind, project_id, COALESCE(thread_id,'') FROM jobs WHERE id=?`, jobID).
		Scan(&kind, &projectID, &threadID)
	if err != nil || kind != queue.KindThreadEval || threadID == "" {
		errJSON(w, 404, "thread evaluation job not found")
		return
	}

	s.telemetryMu.Lock()
	tx, err := s.Telemetry.Begin()
	if err != nil {
		s.telemetryMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	// Idempotent: a retried worker replaces this job's scores, never appends.
	if _, err := tx.Exec(`DELETE FROM thread_scores WHERE job_id=?`, jobID); err != nil {
		s.telemetryMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	now := model.NowUTC()
	for _, sc := range in.Scores {
		if strings.TrimSpace(sc.Name) == "" {
			continue
		}
		value := ""
		if sc.Value != nil {
			if str, ok := sc.Value.(string); ok {
				value = str
			} else {
				value = j(sc.Value)
			}
		}
		source := sc.Source
		if source == "" {
			source = "worker"
		}
		var score interface{}
		if sc.Score != nil {
			score = *sc.Score
		}
		if _, err := tx.Exec(`INSERT INTO thread_scores(
			id, project_id, thread_id, job_id, name, score, value, rationale, source, judge_mode, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			model.NewID(), projectID, threadID, jobID, sc.Name, score, value, sc.Rationale,
			source, sc.JudgeMode, now); err != nil {
			s.telemetryMu.Unlock()
			errJSON(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.telemetryMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	s.telemetryMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"ok": "true", "scores": len(in.Scores), "status": in.Status})
}
