package api

// The bridge between the online engine and the sandbox.
//
// The engine never sees Docker, and the sandbox never sees rules or scoring
// policy: this type resolves a metric name to code, builds the payload from
// stored telemetry, and runs it in isolation. That is the whole boundary.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"evalforge/internal/online"
	"evalforge/internal/sandbox"
)

// customMetricBackend implements online.CustomScorer over the sandbox.
type customMetricBackend struct {
	s    *Server
	run  sandbox.Runner
	time time.Duration
}

// Names lists the project's registered custom metrics.
func (b *customMetricBackend) Names(ctx context.Context, projectID string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := b.s.DB.QueryContext(ctx,
		`SELECT name FROM custom_scorers WHERE project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ScoreTrace builds the payload for one trace and runs the metric.
func (b *customMetricBackend) ScoreTrace(ctx context.Context, projectID, traceID, name string) (online.Score, error) {
	row, err := b.s.customScorerByName(ctx, projectID, name)
	if err != nil {
		if err == sql.ErrNoRows {
			return online.Score{}, fmt.Errorf("custom metric %q is not registered in this project", name)
		}
		return online.Score{}, err
	}
	payload, err := b.tracePayload(ctx, projectID, traceID)
	if err != nil {
		return online.Score{}, err
	}
	return b.execute(ctx, row, payload)
}

// ScoreThread builds the payload for one conversation and runs the metric.
func (b *customMetricBackend) ScoreThread(ctx context.Context, projectID, threadID, name string) (online.Score, error) {
	row, err := b.s.customScorerByName(ctx, projectID, name)
	if err != nil {
		if err == sql.ErrNoRows {
			return online.Score{}, fmt.Errorf("custom metric %q is not registered in this project", name)
		}
		return online.Score{}, err
	}
	payload, err := b.threadPayload(ctx, projectID, threadID)
	if err != nil {
		return online.Score{}, err
	}
	return b.execute(ctx, row, payload)
}

// execute runs the code and maps a sandbox result onto an online score.
func (b *customMetricBackend) execute(ctx context.Context, row customScorerRow, payload map[string]interface{}) (online.Score, error) {
	if !b.run.Available() {
		// Fail closed: no sandbox, no execution. The error names the reason so
		// an operator can tell "Docker is down" from "the metric is broken".
		return online.Score{}, fmt.Errorf("%w: custom metric %q does not run without isolation", sandbox.ErrUnavailable, row.Name)
	}
	timeout := b.time
	if timeout <= 0 {
		timeout = 40 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := b.run.Run(ctx, row.Code, payload)
	if err != nil {
		return online.Score{}, err
	}
	return online.Score{
		Scorer: row.Name, Score: res.Score, Rationale: res.Rationale, JudgeMode: "sandbox",
	}, nil
}

// tracePayload is the data a trace metric sees: the root request/response.
func (b *customMetricBackend) tracePayload(ctx context.Context, projectID, traceID string) (map[string]interface{}, error) {
	var input, output sql.NullString
	err := b.s.Telemetry.QueryRowContext(ctx,
		`SELECT input, output FROM spans
		 WHERE project_id=? AND trace_id=? AND parent_id IS NULL
		 ORDER BY start_ms LIMIT 1`, projectID, traceID).Scan(&input, &output)
	if err != nil {
		return nil, fmt.Errorf("load trace payload: %w", err)
	}
	return map[string]interface{}{
		"trace_id": traceID,
		"input":    parseJSONValue(input.String),
		"output":   parseJSONValue(output.String),
		"expected": "",
	}, nil
}

// threadPayload is the data a conversation metric sees: every turn.
func (b *customMetricBackend) threadPayload(ctx context.Context, projectID, threadID string) (map[string]interface{}, error) {
	rows, err := b.s.Telemetry.QueryContext(ctx,
		`SELECT trace_id, input, output, status, latency_ms FROM spans
		 WHERE project_id=? AND thread_id=? AND parent_id IS NULL
		 ORDER BY start_ms`, projectID, threadID)
	if err != nil {
		return nil, fmt.Errorf("load conversation payload: %w", err)
	}
	defer rows.Close()
	turns := []map[string]interface{}{}
	for rows.Next() {
		var traceID, status string
		var input, output sql.NullString
		var latency int64
		if err := rows.Scan(&traceID, &input, &output, &status, &latency); err != nil {
			return nil, err
		}
		turns = append(turns, map[string]interface{}{
			"trace_id": traceID, "input": parseJSONValue(input.String),
			"output": parseJSONValue(output.String), "status": status, "latency_ms": latency,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"thread_id": threadID, "turns": turns}, nil
}

// parseJSONValue turns a stored JSON value back into a structure. A plain
// string stays a string, which is what a metric author expects to see.
func parseJSONValue(s string) interface{} {
	if s == "" {
		return ""
	}
	return parseJSONPtr(s)
}
