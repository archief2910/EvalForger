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
	cfg.DBPath = database.Path(cfg.DataDir)
	cfg.ServerBaseURL = "http://127.0.0.1:0"
	cfg.MCPAPIBase = cfg.ServerBaseURL

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := database.Migrate(ctx, db, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := database.SeedPrices(ctx, db); err != nil {
		t.Fatalf("seed prices: %v", err)
	}

	s := New(db, cfg, log)
	srv := httptest.NewServer(s.Handler())
	s.SetBaseURL(srv.URL)

	t.Cleanup(func() {
		srv.Close()
		_ = db.Close()
	})
	return &testEnv{srv: srv, s: s}
}

// testServer keeps the OTLP tests terse: they only need the Server and its DB.
func testServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	e := newTestEnvWithAuth(t, false)
	return e.s, e.s.DB
}
