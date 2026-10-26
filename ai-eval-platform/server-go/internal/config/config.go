// Package config loads and validates the process configuration once at boot.
//
// Everything that used to be an os.Getenv call spread across the server lives
// here as a typed field. A bad value fails startup instead of silently
// misrouting evaluations later.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the immutable runtime configuration. Load() is the only
// constructor; callers should pass it by value.
type Config struct {
	Addr            string
	DataDir         string
	DBPath          string // compatibility alias for the control-plane path
	ControlDBPath   string
	TelemetryDBPath string
	LogLevel        slog.Level
	AuthMode        string
	PublicBaseURL   string
	WorkerPath      string
	WorkerTimeout   time.Duration
	JobLease        time.Duration
	ThreadIdle      time.Duration
	PythonBin       string
	LegacyDB        string
	MCPAPIBase      string
	MCPAPIKey       string

	// ServerBaseURL is the origin this process is reachable at, derived from
	// Addr unless overridden. The worker and MCP client use it to call back.
	ServerBaseURL string
}

// Load reads the environment (including a local .env) and returns a validated
// Config.
func Load() (Config, error) {
	LoadDotEnv()

	c := Config{
		Addr:          env("ADDR", ":8000"),
		DataDir:       env("DATA_DIR", filepath.Join("..", "data")),
		AuthMode:      strings.ToLower(env("AUTH_MODE", "off")),
		PublicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		WorkerPath:    env("WORKER_PATH", filepath.Join("..", "worker-py", "worker.py")),
		WorkerTimeout: 12 * time.Minute,
		JobLease:      45 * time.Second,
		ThreadIdle:    30 * time.Minute,
		PythonBin:     env("PYTHON_BIN", "python"),
		LegacyDB:      os.Getenv("LEGACY_DB"),
		MCPAPIKey:     os.Getenv("MCP_API_KEY"),
	}

	// Two independent SQLite files. DBPath is retained as the control-plane
	// alias so older callers and logs keep working.
	c.ControlDBPath = filepath.Join(c.DataDir, "control.db")
	c.TelemetryDBPath = filepath.Join(c.DataDir, "telemetry.db")
	c.DBPath = c.ControlDBPath
	if v := os.Getenv("CONTROL_DB_PATH"); v != "" {
		c.ControlDBPath = v
		c.DBPath = v
	}
	if v := os.Getenv("TELEMETRY_DB_PATH"); v != "" {
		c.TelemetryDBPath = v
	}

	if v := os.Getenv("WORKER_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("WORKER_TIMEOUT_SECONDS must be a positive integer, got %q", v)
		}
		c.WorkerTimeout = time.Duration(n) * time.Second
	}
	if v := os.Getenv("JOB_LEASE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("JOB_LEASE_SECONDS must be a positive integer, got %q", v)
		}
		c.JobLease = time.Duration(n) * time.Second
	}
	if v := os.Getenv("THREAD_IDLE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("THREAD_IDLE_SECONDS must be a positive integer, got %q", v)
		}
		c.ThreadIdle = time.Duration(n) * time.Second
	}

	level := env("LOG_LEVEL", "info")
	if err := c.LogLevel.UnmarshalText([]byte(level)); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL %q is not one of debug, info, warn, error", level)
	}

	c.ServerBaseURL = deriveBaseURL(c.Addr)
	c.MCPAPIBase = strings.TrimRight(os.Getenv("MCP_API_BASE"), "/")
	if c.MCPAPIBase == "" {
		c.MCPAPIBase = c.ServerBaseURL
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate fails fast on values that would otherwise produce a server that
// starts but cannot work.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf("ADDR must not be empty")
	}
	if c.AuthMode != "" && c.AuthMode != "off" && c.AuthMode != "required" {
		return fmt.Errorf("AUTH_MODE must be off or required, got %q", c.AuthMode)
	}
	if c.PublicBaseURL != "" {
		u, err := url.Parse(c.PublicBaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("PUBLIC_BASE_URL must be an absolute URL, got %q", c.PublicBaseURL)
		}
	}
	if c.WorkerTimeout <= 0 {
		return fmt.Errorf("worker timeout must be positive")
	}
	if c.JobLease < time.Second {
		return fmt.Errorf("job lease must be at least 1s")
	}
	if c.ThreadIdle < time.Second {
		return fmt.Errorf("thread idle timeout must be at least 1s")
	}
	if c.WorkerPath == "" {
		return fmt.Errorf("WORKER_PATH must not be empty")
	}
	if c.ControlDBPath != "" && c.TelemetryDBPath != "" &&
		filepath.Clean(c.ControlDBPath) == filepath.Clean(c.TelemetryDBPath) {
		return fmt.Errorf("CONTROL_DB_PATH and TELEMETRY_DB_PATH must be different files")
	}
	return nil
}

// AuthRequired reports whether every non-public route needs a credential.
func (c Config) AuthRequired() bool { return c.AuthMode == "required" }

// Issuer is the public origin used in OAuth discovery documents and token
// audiences. A proxied deployment sets PUBLIC_BASE_URL; otherwise the listen
// address is the best available answer.
func (c Config) Issuer() string {
	if c.PublicBaseURL != "" {
		return c.PublicBaseURL
	}
	return c.ServerBaseURL
}

func deriveBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + addr
}

// LoadDotEnv reads KEY=VALUE lines from a .env file without overriding an
// existing process variable and without logging values. It is intentionally
// forgiving: a missing or malformed file is not a startup failure.
func LoadDotEnv() {
	for _, p := range []string{".env", filepath.Join("..", ".env")} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				continue
			}
			if _, exists := os.LookupEnv(kv[0]); !exists {
				_ = os.Setenv(kv[0], kv[1])
			}
		}
		return
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
