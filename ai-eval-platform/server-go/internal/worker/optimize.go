package worker

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// OptimizeRequest asks the worker to search for a better prompt.
type OptimizeRequest struct {
	OptimizationID   string
	ProjectID        string
	DatasetVersionID string
	BaselinePromptID string
	Scorers          []string
	MaxCandidates    int
}

// RunOptimize executes the optimizer in a worker subprocess.
//
// Like the other modes, the server owns lifecycle and persistence and the
// worker owns compute: it reads the pinned dataset snapshot and the baseline
// prompt over the API, tries the candidates, and reports one JSON document on
// stdout. Optimization is long-running, which is exactly why it goes through
// the durable queue rather than an HTTP handler.
func (c *Client) RunOptimize(parent context.Context, req OptimizeRequest) Outcome {
	max := req.MaxCandidates
	if max <= 0 {
		max = 4
	}
	if max > 8 {
		max = 8
	}
	args := []string{
		c.ScriptPath,
		"--optimize",
		"--project-id", req.ProjectID,
		"--optimization-id", req.OptimizationID,
		"--dataset-version-id", req.DatasetVersionID,
		"--scorers", strings.Join(req.Scorers, ","),
		"--max-candidates", fmt.Sprint(max),
	}
	if req.BaselinePromptID != "" {
		args = append(args, "--prompt-version-id", req.BaselinePromptID)
	}
	// Optimization runs several dataset passes, so it gets a longer budget than
	// a single evaluation would.
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 12 * time.Minute
	}
	return c.run(parent, args, "optimize:"+req.OptimizationID, timeout*2)
}
