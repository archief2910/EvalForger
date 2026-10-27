package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"evalforge/internal/online"
)

// Scorer executes online rules through the worker's `--online` mode.
//
// One subprocess per trace keeps the boundary identical to dataset evaluation:
// the server owns lifecycle and state, the worker owns scorers and provider
// calls. A trace that times out is dropped with a log line; it never blocks
// ingest, because the engine scores off the request path.
type Scorer struct {
	Client  *Client
	Timeout time.Duration
}

// ScoreTrace runs the configured scorers against one stored trace.
func (s *Scorer) ScoreTrace(ctx context.Context, req online.ScoreRequest) ([]online.Score, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("worker client is not configured")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		s.Client.ScriptPath,
		"--online",
		"--trace-id", req.TraceID,
		"--project-id", req.ProjectID,
		"--scorers", strings.Join(req.Scorers, ","),
	}
	return s.runOnlineWorker(ctx, args, timeout)
}

// ScoreThread runs the configured thread scorers against one conversation.
//
// The shape is the same as ScoreTrace: one subprocess, one JSON document on
// stdout, progress on stderr. A conversation is scored as a whole because
// that is what thread scorers judge.
func (s *Scorer) ScoreThread(ctx context.Context, req online.ThreadScoreRequest) ([]online.Score, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("worker client is not configured")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		s.Client.ScriptPath,
		"--online",
		"--thread-id", req.ThreadID,
		"--project-id", req.ProjectID,
		"--scorers", strings.Join(req.Scorers, ","),
	}
	return s.runOnlineWorker(ctx, args, timeout)
}

// runOnlineWorker executes the worker in online mode and parses its result.
func (s *Scorer) runOnlineWorker(ctx context.Context, args []string, timeout time.Duration) ([]online.Score, error) {
	cmd := exec.CommandContext(ctx, s.Client.Python, args...)
	cmd.Env = workerEnv(s.Client.BaseURL, s.Client.InternalToken)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	tail := stderr.String()
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("online scoring timed out after %s", timeout)
	}
	if err != nil {
		if strings.TrimSpace(tail) == "" {
			return nil, fmt.Errorf("online worker: %w", err)
		}
		return nil, fmt.Errorf("online worker: %w: %s", err, tail)
	}

	// The worker writes progress to stderr and exactly one JSON document to
	// stdout, so the last non-empty stdout line is the result.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	payload := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			payload = lines[i]
			break
		}
	}
	if payload == "" {
		return nil, fmt.Errorf("online worker produced no result: %s", tail)
	}
	var out struct {
		Scores []online.Score `json:"scores"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return nil, fmt.Errorf("decode online worker output: %w", err)
	}
	return out.Scores, nil
}
