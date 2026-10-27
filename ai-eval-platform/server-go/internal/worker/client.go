// Package worker runs the Python evaluation subprocess. It is intentionally a
// thin process boundary: the Go server owns lifecycle and status, the worker
// owns provider calls and scoring.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"evalforge/internal/model"
)

// Client executes one worker run per evaluation job.
type Client struct {
	ScriptPath string
	Python     string
	BaseURL    string // this server's origin, passed as GO_SERVER
	Timeout    time.Duration
	// InternalToken authenticates the worker's loopback API calls when auth is
	// required. It is passed only through the child's environment.
	InternalToken string
	Log           *slog.Logger
}

// Request describes one evaluation to execute.
type Request struct {
	ExperimentID string
	ProjectID    string
	Input        model.EvalInput
}

// Outcome is the diagnostic result of one subprocess run. Status transitions
// belong to the queue, not to the worker.
type Outcome struct {
	Diagnostic string
	TimedOut   bool
	// Released means the process is shutting down; the job should go back to
	// the queue without consuming an attempt.
	Released bool
}

// Run executes the worker with a bounded context and returns a log-safe tail.
func (c *Client) Run(parent context.Context, req Request) Outcome {
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()

	args := []string{
		c.ScriptPath,
		"--experiment-id", req.ExperimentID,
		"--project-id", req.ProjectID,
		"--dataset-id", req.Input.DatasetID,
		"--experiment-name", req.Input.ExperimentName,
		"--baseline-id", req.Input.BaselineExperimentID,
		"--scorers", strings.Join(req.Input.ScorerNames, ","),
	}
	if req.Input.DatasetVersionID != "" {
		args = append(args, "--dataset-version-id", req.Input.DatasetVersionID)
	}
	if req.Input.PromptVersionID != "" {
		args = append(args, "--prompt-version-id", req.Input.PromptVersionID)
	}

	cmd := exec.CommandContext(ctx, c.Python, args...)
	cmd.Env = workerEnv(c.BaseURL, c.InternalToken)
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}

	switch {
	case parent.Err() != nil:
		return Outcome{Diagnostic: "released on shutdown", Released: true}
	case ctx.Err() == context.DeadlineExceeded:
		return Outcome{Diagnostic: fmt.Sprintf("worker timed out after %s", c.Timeout), TimedOut: true}
	case err != nil:
		c.Log.Warn("worker failed", "experiment_id", req.ExperimentID, "err", err, "output", tail)
		if strings.TrimSpace(tail) == "" {
			tail = err.Error()
		}
		return Outcome{Diagnostic: tail}
	default:
		c.Log.Info("worker finished", "experiment_id", req.ExperimentID, "output", tail)
		return Outcome{}
	}
}

// RunThread executes one conversation-scoring worker run.
func (c *Client) RunThread(parent context.Context, req ThreadRequest) Outcome {
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()

	args := []string{
		c.ScriptPath,
		"--thread-id", req.ThreadID,
		"--job-id", req.JobID,
		"--project-id", req.ProjectID,
		"--scorers", strings.Join(req.ScorerNames, ","),
	}
	cmd := exec.CommandContext(ctx, c.Python, args...)
	cmd.Env = workerEnv(c.BaseURL, c.InternalToken)
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}

	switch {
	case parent.Err() != nil:
		return Outcome{Diagnostic: "released on shutdown", Released: true}
	case ctx.Err() == context.DeadlineExceeded:
		return Outcome{Diagnostic: fmt.Sprintf("worker timed out after %s", c.Timeout), TimedOut: true}
	case err != nil:
		c.Log.Warn("thread worker failed", "thread_id", req.ThreadID, "err", err, "output", tail)
		if strings.TrimSpace(tail) == "" {
			tail = err.Error()
		}
		return Outcome{Diagnostic: tail}
	default:
		c.Log.Info("thread worker finished", "thread_id", req.ThreadID, "output", tail)
		return Outcome{}
	}
}

// ThreadRequest describes one conversation evaluation.
type ThreadRequest struct {
	JobID       string
	ProjectID   string
	ThreadID    string
	ScorerNames []string
}

// run executes one worker subprocess with an explicit timeout and reports its
// tail. All modes share this so shutdown, timeout and diagnostic handling stay
// identical across them.
func (c *Client) run(parent context.Context, args []string, label string, timeout time.Duration) Outcome {
	if timeout <= 0 {
		timeout = c.Timeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Python, args...)
	cmd.Env = workerEnv(c.BaseURL, c.InternalToken)
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	switch {
	case parent.Err() != nil:
		return Outcome{Diagnostic: "released on shutdown", Released: true}
	case ctx.Err() == context.DeadlineExceeded:
		return Outcome{Diagnostic: fmt.Sprintf("worker timed out after %s", timeout), TimedOut: true}
	case err != nil:
		c.Log.Warn("worker failed", "label", label, "err", err, "output", tail)
		if strings.TrimSpace(tail) == "" {
			tail = err.Error()
		}
		return Outcome{Diagnostic: tail}
	default:
		c.Log.Info("worker finished", "label", label, "output", tail)
		return Outcome{}
	}
}

// workerEnv returns the process environment with GO_SERVER pointed at this
// server, so a non-default ADDR (or a second instance) still reports back here.
// The internal token is injected last so an inherited value cannot override it.
func workerEnv(selfBase, internalToken string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GO_SERVER=") || strings.HasPrefix(kv, "EVALFORGE_INTERNAL_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GO_SERVER="+selfBase)
	if internalToken != "" {
		env = append(env, "EVALFORGE_INTERNAL_TOKEN="+internalToken)
	}
	return env
}
