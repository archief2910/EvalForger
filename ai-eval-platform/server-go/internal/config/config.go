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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the immutable runtime configuration. Load() is the only
// constructor; callers should pass it by value.
type Config struct {
	Addr          string
	DataDir       string
	ControlURL    string // PostgreSQL DSN for the control plane
	ControlSchema string // schema inside that database; tests use their own
	TelemetryPath string // DuckDB file for the telemetry plane
	LogLevel      slog.Level
	AuthMode      string
	PublicBaseURL string
	WorkerPath    string
	WorkerTimeout time.Duration
	JobLease      time.Duration
	ThreadIdle    time.Duration
	// Online evaluation runs off the ingest path. Concurrency bounds provider
	// load; Timeout bounds one trace so a slow judge cannot pin a slot forever.
	OnlineConcurrency int
	OnlineTimeout     time.Duration
	PythonBin         string
	LegacyDB          string
	MCPAPIBase        string
	MCPAPIKey         string

	// ServerBaseURL is the origin this process is reachable at, derived from
	// Addr unless overridden. The worker and MCP client use it to call back.
	ServerBaseURL string
}

// Load reads the environment (including a local .env) and returns a validated
// Config.
func Load() (Config, error) {
	LoadDotEnv()

	c := Config{
		Addr:              env("ADDR", ":8000"),
		DataDir:           env("DATA_DIR", filepath.Join("..", "data")),
		AuthMode:          strings.ToLower(env("AUTH_MODE", "off")),
		PublicBaseURL:     strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		WorkerPath:        env("WORKER_PATH", filepath.Join("..", "worker-py", "worker.py")),
		WorkerTimeout:     12 * time.Minute,
		JobLease:          45 * time.Second,
		ThreadIdle:        30 * time.Minute,
		OnlineConcurrency: 2,
		OnlineTimeout:     3 * time.Minute,
		PythonBin:         env("PYTHON_BIN", "python"),
		LegacyDB:          os.Getenv("LEGACY_DB"),
		MCPAPIKey:         os.Getenv("MCP_API_KEY"),
	}

	// Two engines, matching the plane split: PostgreSQL holds the small,
	// transactional metadata; DuckDB holds append-heavy telemetry on disk.
	c.ControlURL = env("CONTROL_DB_URL", DefaultControlURL)
	c.ControlSchema = env("CONTROL_DB_SCHEMA", "public")
	c.TelemetryPath = env("TELEMETRY_DB_PATH", filepath.Join(c.DataDir, "telemetry.duckdb"))

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
	if v := os.Getenv("ONLINE_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 32 {
			return Config{}, fmt.Errorf("ONLINE_CONCURRENCY must be an integer between 1 and 32, got %q", v)
		}
		c.OnlineConcurrency = n
	}
	if v := os.Getenv("ONLINE_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("ONLINE_TIMEOUT_SECONDS must be a positive integer, got %q", v)
		}
		c.OnlineTimeout = time.Duration(n) * time.Second
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
	if c.ControlURL == "" {
		return fmt.Errorf("CONTROL_DB_URL must not be empty")
	}
	if u, err := url.Parse(c.ControlURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("CONTROL_DB_URL must be a postgres:// DSN, got %q", c.ControlURL)
	}
	if !schemaName.MatchString(c.ControlSchema) {
		return fmt.Errorf("CONTROL_DB_SCHEMA must be a lowercase identifier, got %q", c.ControlSchema)
	}
	if strings.TrimSpace(c.TelemetryPath) == "" {
		return fmt.Errorf("TELEMETRY_DB_PATH must not be empty")
	}
	return nil
}

// DefaultControlURL is the local development database. It points at a loopback
// PostgreSQL with trust authentication, the same shape as the bundled pgdata
// directory; a deployment sets CONTROL_DB_URL.
const DefaultControlURL = "postgres://evalforge@127.0.0.1:55432/evalforge?sslmode=disable"

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

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
