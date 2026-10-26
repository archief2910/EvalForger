package api

// Shared test harness. Tests construct the same Server object the binary does,
// against a temporary SQLite database, and drive it over a real httptest
// listener so middleware, routing and auth are exercised together.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
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
	cfg.ControlDBPath = database.Path(cfg.DataDir)
	cfg.TelemetryDBPath = database.TelemetryPath(cfg.DataDir)
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
	})
	return &testEnv{srv: srv, s: s}
}

// testServer keeps the OTLP tests terse: they only need the Server and the
// telemetry DB where spans land.
func testServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	return e.s, e.s.Telemetry
}
