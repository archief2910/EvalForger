// Package sandbox runs user-supplied Python metrics.
//
// This is the only place in EvalForge that executes code a user wrote. The
// trusted worker runs a fixed registry; a custom metric is arbitrary Python,
// so it gets a boundary of its own rather than being folded into the worker.
//
// The boundary is a disposable container:
//
//	docker run --rm --network none --read-only --user 65534:65534 \
//	  --memory 256m --pids-limit 64 --cpus 1 --cap-drop ALL \
//	  --security-opt no-new-privileges \
//	  -v <tmp>/scorer.py:/sandbox/scorer.py:ro \
//	  -v <tmp>/payload.json:/sandbox/payload.json:ro \
//	  python:3.12-slim python /sandbox/runner.py
//
// No network means a metric cannot exfiltrate the trace it is scoring. A
// read-only root filesystem means it cannot persist. The PID and memory caps
// mean a fork bomb or an allocation loop kills the container, not the host.
//
// Fail closed. If Docker is not reachable, a custom metric does not run: it
// returns an error and the caller records it. A hardened subprocess on the
// host is *not* a security boundary, so there is no silent local fallback.
package sandbox

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed runner.py
var runnerScript []byte

// Result is one custom metric's verdict.
type Result struct {
	Score     *float64 `json:"score"`
	Rationale string   `json:"rationale"`
	Error     string   `json:"error,omitempty"`
}

// Runner executes untrusted code. The interface exists so the evaluation path
// can be tested without Docker and so a deployment can swap in a different
// isolation backend without touching the callers.
type Runner interface {
	// Run executes one metric and returns its verdict. A non-nil error means
	// the code did not run or did not produce a usable result.
	Run(ctx context.Context, code string, payload map[string]interface{}) (Result, error)
	// Available reports whether this runner can execute code right now.
	Available() bool
	// Describe names the isolation backend, so the API can tell a user what
	// their code will run inside.
	Describe() string
}

// ErrUnavailable means the isolation backend is not usable. Callers must treat
// it as a refusal, never as "run it somewhere else".
var ErrUnavailable = errors.New("sandbox is unavailable")

// DockerRunner executes metrics in a disposable container.
type DockerRunner struct {
	// Image is the interpreter image. It must have Python 3 and nothing the
	// metric is allowed to depend on beyond the standard library.
	Image string
	// Timeout bounds one execution end to end.
	Timeout time.Duration
	// MemoryMB, PIDs and CPUs are the container resource caps.
	MemoryMB int
	PIDs     int
	CPUs     string
	// WorkDir is where the metric and its payload are staged before the run.
	//
	// It matters when the server itself runs in a container against a mounted
	// Docker socket: the daemon resolves `-v` paths on the host, not inside
	// this process, so the staging directory must be a path the host and the
	// container agree on. Deployments mount one directory at the same absolute
	// path on both sides (see docker-compose.yml); the default is the local
	// temp directory, which is correct when the server runs on the host.
	WorkDir string

	probeMu   sync.Mutex
	probedAt  time.Time
	available bool
	probeErr  error
}

// NewDockerRunner returns a runner with conservative defaults.
func NewDockerRunner() *DockerRunner {
	return &DockerRunner{
		Image:    envOr("SANDBOX_IMAGE", "python:3.12-slim"),
		Timeout:  30 * time.Second,
		MemoryMB: 256,
		PIDs:     64,
		CPUs:     "1",
		WorkDir:  envOr("SANDBOX_WORK_DIR", os.TempDir()),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Describe names the backend for the API.
func (d *DockerRunner) Describe() string {
	return "docker:" + d.Image
}

// Available probes the Docker daemon. The result is cached for a minute: a
// probe starts a process, and the evaluation loop should not pay for one per
// metric.
func (d *DockerRunner) Available() bool {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if !d.probedAt.IsZero() && time.Since(d.probedAt) < time.Minute {
		return d.available
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run()
	d.probedAt = time.Now()
	d.available = err == nil
	d.probeErr = err
	return d.available
}

// probeError explains why the backend is unavailable.
func (d *DockerRunner) probeError() error {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.probeErr == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, d.probeErr)
}

// Run executes the metric inside a container.
func (d *DockerRunner) Run(ctx context.Context, code string, payload map[string]interface{}) (Result, error) {
	if strings.TrimSpace(code) == "" {
		return Result{}, errors.New("metric code is empty")
	}
	if !d.Available() {
		return Result{}, d.probeError()
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workRoot := d.WorkDir
	if workRoot == "" {
		workRoot = os.TempDir()
	}
	dir, err := os.MkdirTemp(workRoot, "evalforge-sandbox-")
	if err != nil {
		return Result{}, fmt.Errorf("sandbox temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	// MkdirTemp creates 0700, and the container runs as an unprivileged user
	// that must be able to traverse this directory to read the staged files.
	if err := os.Chmod(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("sandbox chmod: %w", err)
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("encode payload: %w", err)
	}
	files := map[string][]byte{
		"runner.py":    runnerScript,
		"scorer.py":    []byte(code),
		"payload.json": payloadJSON,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o444); err != nil {
			return Result{}, fmt.Errorf("sandbox write %s: %w", name, err)
		}
	}

	args := []string{
		"run", "--rm",
		"--network", "none",
		"--read-only",
		"--user", "65534:65534",
		"--memory", strconv.Itoa(d.MemoryMB) + "m",
		"--pids-limit", strconv.Itoa(d.PIDs),
		"--cpus", d.CPUs,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"-v", dir + ":/sandbox:ro",
		"--workdir", "/sandbox",
		d.Image,
		"python", "/sandbox/runner.py",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("metric timed out after %s", timeout)
	}
	if err != nil {
		// The runner reports a user error as JSON on stdout and exits non-zero.
		// Prefer that message: it names the user's mistake, not the container's.
		if res, parseErr := parseResult(stdout.Bytes()); parseErr == nil && res.Error != "" {
			return res, fmt.Errorf("metric failed: %s", res.Error)
		}
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return Result{}, fmt.Errorf("sandbox exited: %v: %s", err, tail)
	}
	res, err := parseResult(stdout.Bytes())
	if err != nil {
		return Result{}, err
	}
	if res.Error != "" {
		return res, fmt.Errorf("metric failed: %s", res.Error)
	}
	return res, nil
}

// parseResult decodes the runner's single JSON document.
func parseResult(out []byte) (Result, error) {
	text := strings.TrimSpace(string(out))
	lines := strings.Split(text, "\n")
	payload := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			payload = lines[i]
			break
		}
	}
	if payload == "" {
		return Result{}, errors.New("sandbox produced no result")
	}
	var res Result
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		return Result{}, fmt.Errorf("decode sandbox result: %w", err)
	}
	if res.Score != nil && (*res.Score < 0 || *res.Score > 1) {
		return Result{}, fmt.Errorf("metric score %v is outside [0,1]", *res.Score)
	}
	return res, nil
}

// LocalRunner exists for development and refuses every request unless the
// operator has explicitly opted in. It is not a sandbox: a subprocess with a
// timeout is not an isolation boundary, and pretending otherwise is how a
// security model rots.
type LocalRunner struct {
	// AllowUnsafe turns a refusal into a best-effort subprocess run. It exists
	// so a developer can iterate on a metric on a laptop without Docker; it is
	// never enabled by default.
	AllowUnsafe bool
	Python      string
	Timeout     time.Duration
}

// Describe names the backend.
func (l *LocalRunner) Describe() string { return "local-subprocess" }

// Available reports whether this runner will execute anything.
func (l *LocalRunner) Available() bool { return l.AllowUnsafe }

// Run refuses unless explicitly allowed.
func (l *LocalRunner) Run(ctx context.Context, code string, payload map[string]interface{}) (Result, error) {
	if !l.AllowUnsafe {
		return Result{}, fmt.Errorf("%w: custom metrics need Docker; set SANDBOX_ALLOW_UNSAFE_LOCAL=1 only for local development",
			ErrUnavailable)
	}
	python := l.Python
	if python == "" {
		python = "python"
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "evalforge-local-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	payloadJSON, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(dir, "scorer.py"), []byte(code), 0o600); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.json"), payloadJSON, 0o600); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "runner.py"), runnerScript, 0o600); err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, python, filepath.Join(dir, "runner.py"), dir)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{}, fmt.Errorf("metric timed out after %s", timeout)
		}
		if res, perr := parseResult(stdout.Bytes()); perr == nil && res.Error != "" {
			return res, fmt.Errorf("metric failed: %s", res.Error)
		}
		return Result{}, fmt.Errorf("metric failed: %v", err)
	}
	return parseResult(stdout.Bytes())
}
