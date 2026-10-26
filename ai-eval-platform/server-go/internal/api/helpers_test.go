package api

// Shared test harness. Tests construct the same Server object the binary does,
// against a private PostgreSQL schema and a temporary DuckDB file, and drive it
// over a real httptest listener so middleware, routing and auth are exercised
// together.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"evalforge/internal/config"
	"evalforge/internal/database"
)

type testEnv struct {
	srv *httptest.Server
	s   *Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWithAuth(t, true)
}

func newTestEnvWithAuth(t *testing.T, auth bool) *testEnv {
	t.Helper()
	cfg := config.Config{
		Addr:          ":0",
		DataDir:       t.TempDir(),
		AuthMode:      "off",
		WorkerPath:    "unused-worker.py",
		WorkerTimeout: time.Minute,
		JobLease:      45 * time.Second,
		PythonBin:     "python",
	}
	if auth {
		cfg.AuthMode = "required"
	}
	cfg.ControlURL = testControlURL()
	cfg.ControlSchema = uniqueSchema()
	cfg.TelemetryPath = filepath.Join(cfg.DataDir, "telemetry.duckdb")
	cfg.ServerBaseURL = "http://127.0.0.1:0"
	cfg.MCPAPIBase = cfg.ServerBaseURL

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stores, err := database.OpenStores(ctx, cfg, log)
	if err != nil {
		t.Fatalf("open stores: %v", err)
	}

	s := New(stores.Control, stores.Telemetry, cfg, log)
	srv := httptest.NewServer(s.Handler())
	s.SetBaseURL(srv.URL)

	t.Cleanup(func() {
		srv.Close()
		_ = stores.Control.Close()
		_ = stores.Telemetry.Close()
		if err := database.DropSchema(context.Background(), cfg); err != nil {
			t.Logf("drop test schema %s: %v", cfg.ControlSchema, err)
		}
	})
	return &testEnv{srv: srv, s: s}
}

// testControlURL points at the local development PostgreSQL unless the
// environment overrides it (CI, or a different port).
func testControlURL() string {
	if v := os.Getenv("TEST_CONTROL_DB_URL"); v != "" {
		return v
	}
	return config.DefaultControlURL
}

var schemaSeq atomic.Uint64

// uniqueSchema gives every test its own schema, so two tests can never see
// each other's rows and a failed test leaves nothing behind.
func uniqueSchema() string {
	return fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
}

// testServer keeps the OTLP tests terse: they only need the Server and the
// telemetry DB where spans land.
func testServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	return e.s, e.s.Telemetry
}
